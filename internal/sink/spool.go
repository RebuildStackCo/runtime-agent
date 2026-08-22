// Package sink writes payload batches to the local spool. The local sink is
// the schema contract: it writes exactly the bytes the backend sink will
// transmit (docs/development.md), so the golden files over its output are
// the enforcement mechanism for the payload schema. The spool directory's
// durability is an installation choice, not a requirement (ADR 0007); its
// layout follows ADR 0003 — filenames are natural keys, deletion is
// acknowledgment, no index.
package sink

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/inventory"
	"github.com/RebuildStackCo/runtime-agent/internal/journal"
	"github.com/RebuildStackCo/runtime-agent/internal/metadata"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// DefaultMaxAge caps how long an unacknowledged payload may sit in the
// spool. Past it, files are removed even without acknowledgment: an
// extended outage degrades to memory-only behavior instead of filling the
// volume (ADR 0007).
const DefaultMaxAge = 24 * time.Hour

// Provenance classes. Every payload declares how its facts were obtained, so
// the backend can never merge epistemically different data under one natural
// key: a value read from a spec is not the same kind of claim as a value the
// kubelet measured or a profiler sampled. The discriminator is per payload
// kind because a kind has exactly one provenance.
const (
	// SourceStructural is read from an object's spec or status: declared,
	// deterministic, true the moment it is read and independent of any
	// sampling.
	SourceStructural = "structural"
	// SourceMeasured is obtained by polling an instrument — the kubelet — and
	// is therefore subject to scrape failure. A measured payload carries its
	// own observation state so a failed scrape is never read as a quiet
	// container.
	SourceMeasured = "measured"
	// SourceJournal is derived from object metadata that records history:
	// conditions, restart counts, terminations.
	SourceJournal = "journal"
	// SourceSampled is statistical: a profiler observed a fraction of the
	// instants in a window and the payload is an estimate, not a total. The
	// class says what kind of claim the facts are, never how they were
	// captured — which capture produced them is what the kind names
	// (`ebpf_profile`), and a profile pulled from a /debug/pprof endpoint would
	// be a different kind of the same class (ADR 0023).
	SourceSampled = "sampled"
)

// Spool writes payload files into one directory. Methods may be called from
// different goroutines as long as no two writers target the same natural
// key — which the agent's wiring guarantees (one usage poller, one OOM
// reporter).
type Spool struct {
	dir    string
	maxAge time.Duration
}

// NewSpool opens (creating if needed) the spool directory. maxAge ≤ 0
// selects DefaultMaxAge.
func NewSpool(dir string, maxAge time.Duration) (*Spool, error) {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating spool directory: %w", err)
	}
	return &Spool{dir: dir, maxAge: maxAge}, nil
}

// usagePayload is one shippable batch: every record of one wall-clock
// window. Kind "usage_snapshot" is an open-window snapshot that supersedes
// its predecessor; "usage_window" is the final record of a closed window.
//
// Observation travels with the records because these are measured facts: a
// container that reports nothing because it was idle and one that reports
// nothing because every scrape of its node failed produce identical records,
// and only the agent can tell them apart (ADR 0012).
type usagePayload struct {
	Kind          string                `json:"kind"`
	Source        string                `json:"source"`
	Sequence      int64                 `json:"sequence,omitempty"`
	WindowStart   time.Time             `json:"window_start"`
	WindowSeconds int64                 `json:"window_seconds"`
	Observation   collector.Observation `json:"observation"`
	Records       []*rollup.Record      `json:"records"`
}

// oomPayload is one OOM kill event; events bypass windows and ship
// immediately (ADR 0006).
type oomPayload struct {
	Kind   string            `json:"kind"`
	Source string            `json:"source"`
	Event  collector.OOMKill `json:"event"`
}

// goInventoryPayload is the current Go inventory of the cluster — one record
// per (namespace, workload, container), joined from node facts (ADR 0010). It
// is a single superseding batch: each write replaces the previous one under its
// fixed natural key (the payload kind), the on-disk mirror of the backend's
// upsert-by-key ingest. The sequence orders supersedes, never arrival time.
//
// CapturedAt dates the assembly of the snapshot, not each fact in it: the node
// facts were collected by scans that finished at various moments before it.
// How much of the fleet those scans covered is what Coverage answers.
type goInventoryPayload struct {
	Kind       string               `json:"kind"`
	Source     string               `json:"source"`
	Sequence   int64                `json:"sequence,omitempty"`
	CapturedAt time.Time            `json:"captured_at"`
	Coverage   inventory.Coverage   `json:"coverage"`
	Records    []inventory.GoRecord `json:"records"`
}

// goBuildPayload is what one build is made of and how it was built, keyed by its
// image digest: the toolchain, the dependency module set, and the allow-listed
// build settings. Unlike every other structural payload it neither supersedes
// nor carries a sequence or a capture time — these are properties of the build,
// fixed the moment the image was produced, so the payload is immutable given its
// key and a redelivery is byte-identical (ADR 0017).
//
// Settings may be absent: the toolchain records vcs.* only when it can, which in
// container builds is the exception rather than the rule (ADR 0019).
type goBuildPayload struct {
	Kind        string            `json:"kind"`
	Source      string            `json:"source"`
	ImageDigest string            `json:"image_digest"`
	GoVersion   string            `json:"go_version"`
	MainModule  string            `json:"main_module"`
	Modules     []string          `json:"modules"`
	Settings    map[string]string `json:"settings,omitempty"`
}

// containerRestartsPayload is every collected container's restart history
// within one window: one file per window holding many records, exactly as a
// usage snapshot is (ADR 0020).
//
// Grouping by window rather than filing one payload per restart is the decision
// the shape encodes. A per-event payload would put the spool's file count under
// the control of a crash loop — a hundred containers in CrashLoopBackOff would
// write thousands of files an hour into a spool bounded only by age — while the
// question a restart answers ("how often, and why") is a property of the window,
// not of the individual restart.
//
// It supersedes within its window: each flush replaces the window's file, and
// the last write before the window closes is its final value. Unlike usage
// there is no separate closed-window kind, because a restart count only grows
// and the final write needs no different shape.
type containerRestartsPayload struct {
	Kind          string                  `json:"kind"`
	Source        string                  `json:"source"`
	Sequence      int64                   `json:"sequence,omitempty"`
	WindowStart   time.Time               `json:"window_start"`
	WindowSeconds int64                   `json:"window_seconds"`
	Records       []journal.RestartRecord `json:"records"`
}

// podDisruptionsPayload is every pod the cluster removed within one window:
// preempted to make room, evicted under node pressure, drained, or deleted
// through the eviction API (ADR 0021). One file per window holding many
// records, like the restart journal.
//
// Windows here are a delivery boundary rather than an aggregation: a pod is
// disrupted once, so each record stands alone and carries the instant
// Kubernetes recorded. Grouping them keeps the spool's file count bounded by
// time rather than by how bad an incident is — a node evicting forty pods must
// not write forty files.
type podDisruptionsPayload struct {
	Kind          string                     `json:"kind"`
	Source        string                     `json:"source"`
	Sequence      int64                      `json:"sequence,omitempty"`
	WindowStart   time.Time                  `json:"window_start"`
	WindowSeconds int64                      `json:"window_seconds"`
	Records       []journal.DisruptionRecord `json:"records"`
}

// workloadMetadataPayload is the declared shape of every collected workload
// container — requests, limits, QoS, ports — and where its replicas run. Like
// go-inventory it is a single superseding batch under a fixed natural key (the
// payload kind): it describes current cluster state, so the newest snapshot
// replaces its predecessor rather than accumulating. It carries no window
// because a spec has no window — a spec is not observed over time — but it does
// carry the instant it was taken, so a consumer can tell how old the newest
// snapshot is without inferring it from delivery (ADR 0017, amending ADR 0012 §3).
type workloadMetadataPayload struct {
	Kind       string            `json:"kind"`
	Source     string            `json:"source"`
	Sequence   int64             `json:"sequence,omitempty"`
	CapturedAt time.Time         `json:"captured_at"`
	Records    []metadata.Record `json:"records"`
}

// nodeMetadataPayload is the current node inventory: size, instance and
// capacity type, and topology. It is the join target for every placement fact —
// a workload-metadata record names the nodes its replicas sit on, and this
// payload is what turns those node names into zones and instance types.
// Superseding batch, same reasoning as workloadMetadataPayload.
type nodeMetadataPayload struct {
	Kind       string               `json:"kind"`
	Source     string               `json:"source"`
	Sequence   int64                `json:"sequence,omitempty"`
	CapturedAt time.Time            `json:"captured_at"`
	Nodes      []collector.NodeInfo `json:"nodes"`
}

// profilePayload is one captured eBPF CPU profile: the allow-list-filtered,
// symbolized pprof bytes (gzipped protobuf, base64 in JSON) plus the natural key
// that identifies which workload and capture window it belongs to. Unlike
// go-inventory it does not supersede: each capture is a distinct window
// (ProfileKey.CaptureStart–CaptureEnd), so profiles accumulate and are bounded by
// the maxAge sweep, never overwritten (ADR 0011 §6). The pprof bytes were reduced
// to allowed frames on the node before this payload was formed (ADR 0011 §4).
type profilePayload struct {
	Kind         string    `json:"kind"`
	Namespace    string    `json:"namespace"`
	Workload     string    `json:"workload"`
	Container    string    `json:"container"`
	ImageDigest  string    `json:"image_digest,omitempty"`
	CaptureStart time.Time `json:"capture_start"`
	CaptureEnd   time.Time `json:"capture_end"`
	Source       string    `json:"source"`
	Pprof        []byte    `json:"pprof"`
}

// ProfileKey identifies a captured profile. The namespace/workload/image-digest
// are the controller's join of the node's container ID (ADR 0011 §6, the ADR
// 0010 pattern); the image digest and the capture interval together are what
// make a capture distinguishable from the others of its workload in the same
// window (ADR 0023).
type ProfileKey struct {
	Namespace    string
	Workload     string
	Container    string
	ImageDigest  string
	CaptureStart time.Time
	CaptureEnd   time.Time
}

type windowKey struct {
	start   time.Time
	seconds int64
}

func groupByWindow(records []*rollup.Record) map[windowKey][]*rollup.Record {
	grouped := make(map[windowKey][]*rollup.Record)
	for _, r := range records {
		k := windowKey{start: r.WindowStart, seconds: r.WindowSeconds}
		grouped[k] = append(grouped[k], r)
	}
	return grouped
}

func (w windowKey) name() string {
	return fmt.Sprintf("usage-%d-%d", w.start.Unix(), w.seconds)
}

// WriteUsageSnapshot writes open-window snapshots, one file per window,
// atomically replacing that window's previous snapshot — the on-disk mirror
// of the backend's supersede-by-key ingest. It also sweeps expired files,
// riding the snapshot cadence so no extra timer exists.
func (s *Spool) WriteUsageSnapshot(sequence int64, records []*rollup.Record, obs collector.Observation) error {
	for k, group := range groupByWindow(records) {
		payload := usagePayload{
			Kind:          "usage_snapshot",
			Source:        SourceMeasured,
			Sequence:      sequence,
			WindowStart:   k.start,
			WindowSeconds: k.seconds,
			Observation:   obs,
			Records:       group,
		}
		if err := s.write(k.name()+".snapshot.json", payload); err != nil {
			return err
		}
	}
	return s.sweep(time.Now())
}

// WriteClosedWindows writes final closed-window records, one file per
// window, and removes the window's snapshot file — the closed record
// supersedes every snapshot.
func (s *Spool) WriteClosedWindows(records []*rollup.Record, obs collector.Observation) error {
	for k, group := range groupByWindow(records) {
		payload := usagePayload{
			Kind:          "usage_window",
			Source:        SourceMeasured,
			WindowStart:   k.start,
			WindowSeconds: k.seconds,
			Observation:   obs,
			Records:       group,
		}
		if err := s.write(k.name()+".json", payload); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(s.dir, k.name()+".snapshot.json")); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing superseded snapshot: %w", err)
		}
	}
	return nil
}

// WriteContainerRestarts writes the restart records of each window they belong
// to, one file per window, atomically replacing that window's previous file.
// records must already be in a deterministic order (the accumulator sorts them)
// so the payload bytes are stable — the golden contract.
//
// A window with no restarts writes nothing: the accumulator only holds records
// for containers that actually restarted, and an empty payload would say
// "observed and none" for every window of every quiet cluster.
func (s *Spool) WriteContainerRestarts(sequence int64, records []journal.RestartRecord) error {
	grouped := make(map[windowKey][]journal.RestartRecord)
	for _, r := range records {
		k := windowKey{start: r.WindowStart, seconds: r.WindowSeconds}
		grouped[k] = append(grouped[k], r)
	}
	for k, group := range grouped {
		payload := containerRestartsPayload{
			Kind:          "container_restarts",
			Source:        SourceJournal,
			Sequence:      sequence,
			WindowStart:   k.start,
			WindowSeconds: k.seconds,
			Records:       group,
		}
		if err := s.write(fmt.Sprintf("restarts-%d-%d.json", k.start.Unix(), k.seconds), payload); err != nil {
			return err
		}
	}
	return nil
}

// WritePodDisruptions writes the disruption records of each window they belong
// to, one file per window, atomically replacing that window's previous file.
// records must already be in a deterministic order (the accumulator sorts them)
// so the payload bytes are stable — the golden contract.
//
// A window with no disruptions writes nothing. A cluster where nothing was
// preempted or evicted has nothing to say, and an empty payload would claim
// otherwise for every quiet hour of every cluster.
func (s *Spool) WritePodDisruptions(sequence int64, records []journal.DisruptionRecord) error {
	grouped := make(map[windowKey][]journal.DisruptionRecord)
	for _, r := range records {
		k := windowKey{start: r.WindowStart, seconds: r.WindowSeconds}
		grouped[k] = append(grouped[k], r)
	}
	for k, group := range grouped {
		payload := podDisruptionsPayload{
			Kind:          "pod_disruptions",
			Source:        SourceJournal,
			Sequence:      sequence,
			WindowStart:   k.start,
			WindowSeconds: k.seconds,
			Records:       group,
		}
		if err := s.write(fmt.Sprintf("disruptions-%d-%d.json", k.start.Unix(), k.seconds), payload); err != nil {
			return err
		}
	}
	return nil
}

// WriteOOMKill writes one OOM event immediately. The filename carries the
// event's natural identity, so a re-observed event overwrites rather than
// duplicates.
func (s *Spool) WriteOOMKill(e collector.OOMKill) error {
	name := fmt.Sprintf("oom-%d-%s-%s-%s-%d.json",
		e.FinishedAt.Unix(), e.Namespace, e.Pod, e.Container, e.RestartCount)
	return s.write(name, oomPayload{Kind: "oom_kill", Source: SourceJournal, Event: e})
}

// WriteGoInventory writes the current Go inventory as one superseding batch
// (ADR 0010). One file per cluster, atomically replaced each flush: the newest
// inventory supersedes its predecessor, exactly as an open-window usage
// snapshot does. records must already be in a deterministic order (the store
// sorts them) so the payload bytes are stable — the golden contract. capturedAt
// is passed in rather than read from the clock for the same reason.
func (s *Spool) WriteGoInventory(sequence int64, capturedAt time.Time, cov inventory.Coverage, records []inventory.GoRecord) error {
	payload := goInventoryPayload{
		Kind:       "go_inventory",
		Source:     SourceStructural,
		Sequence:   sequence,
		CapturedAt: capturedAt.UTC(),
		Coverage:   cov,
		Records:    records,
	}
	return s.write("go-inventory.json", payload)
}

// WriteGoBuild writes one build's facts. Unlike the superseding go-inventory,
// each build gets its own file keyed by image digest: the facts never change for
// a given digest, so the write is idempotent and the controller only issues it
// once per build (ADR 0017). The maxAge sweep bounds how many accumulate,
// exactly as it does for profiles.
func (s *Spool) WriteGoBuild(b inventory.BuildFacts) error {
	if b.ImageDigest == "" {
		return fmt.Errorf("build facts have no image digest")
	}
	modules := b.Modules
	if modules == nil {
		modules = []string{}
	}
	payload := goBuildPayload{
		Kind:        "go_build",
		Source:      SourceStructural,
		ImageDigest: b.ImageDigest,
		GoVersion:   b.GoVersion,
		MainModule:  b.MainModule,
		Modules:     modules,
		Settings:    b.Settings,
	}
	return s.write("go-build-"+digestFileToken(b.ImageDigest)+".json", payload)
}

// digestFileToken turns an image digest into a filename-safe token. Digests are
// "algorithm:hex", and the colon is legal on the platforms the agent runs on but
// hostile to shell and archive tooling; anything outside the digest alphabet is
// replaced so a malformed digest can never escape the spool directory.
func digestFileToken(digest string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, digest)
}

// WriteWorkloadMetadata writes the current workload metadata as one superseding
// batch. records must already be in a deterministic order (metadata.Aggregate
// sorts them) so the payload bytes are stable — the golden contract.
func (s *Spool) WriteWorkloadMetadata(sequence int64, capturedAt time.Time, records []metadata.Record) error {
	payload := workloadMetadataPayload{
		Kind:       "workload_metadata",
		Source:     SourceStructural,
		Sequence:   sequence,
		CapturedAt: capturedAt.UTC(),
		Records:    records,
	}
	return s.write("workload-metadata.json", payload)
}

// WriteNodeMetadata writes the current node inventory as one superseding batch.
// nodes must already be sorted by name (NodeWatcher.Nodes does it).
func (s *Spool) WriteNodeMetadata(sequence int64, capturedAt time.Time, nodes []collector.NodeInfo) error {
	payload := nodeMetadataPayload{
		Kind:       "node_metadata",
		Source:     SourceStructural,
		Sequence:   sequence,
		CapturedAt: capturedAt.UTC(),
		Nodes:      nodes,
	}
	return s.write("node-metadata.json", payload)
}

// WriteProfile writes one captured eBPF CPU profile. Unlike the superseding
// go-inventory, each capture is its own file: profiles accumulate and are
// bounded by the maxAge sweep, never overwritten (ADR 0011 §6). The pprof bytes
// must already be allow-list-filtered and validated (ADR 0011 §4–5).
//
// The filename carries the whole natural key, image digest included. Without it
// two captures of one workload's container in one window — two replicas on the
// same node, or two builds during a rollout — produce the same name and the
// second silently replaces the first, since the node cuts every window on the
// same boundaries and ships one report per container (ADR 0023).
func (s *Spool) WriteProfile(key ProfileKey, pprof []byte) error {
	payload := profilePayload{
		Kind:         "ebpf_profile",
		Namespace:    key.Namespace,
		Workload:     key.Workload,
		Container:    key.Container,
		ImageDigest:  key.ImageDigest,
		CaptureStart: key.CaptureStart,
		CaptureEnd:   key.CaptureEnd,
		Source:       SourceSampled,
		Pprof:        pprof,
	}
	name := fmt.Sprintf("profile-%s-%s-%s-%s-%d-%d.json",
		key.Namespace, key.Workload, key.Container, shortDigest(key.ImageDigest),
		key.CaptureStart.Unix(), key.CaptureEnd.Unix())
	return s.write(name, payload)
}

// shortDigestLen is how much of a digest the filename carries. A digest is
// `algorithm:hex`; twelve hex characters is the conventional short form and is
// what a human recognizes from `docker images`. Two distinct builds sharing a
// twelve-character prefix is not a risk worth pricing — and unlike ADR 0019's
// rule against truncating collected values, nothing is lost here: the full
// digest is in the payload, and this is a local file name, not a fact that
// leaves the cluster.
const shortDigestLen = 12

// noDigest names the state of a capture that cannot be tied to a build. For
// this payload it is close to impossible — sampling a container's CPU means the
// container was running, so the kubelet had resolved its digest — and it means
// the controller's informer had not yet caught up, never that the container had
// not started. It is a literal rather than an empty segment so a directory
// listing shows the state instead of a gap that reads like a bug.
const noDigest = "nodigest"

// shortDigest reduces an image digest to a filename-safe short form. Only hex
// after the algorithm separator survives, so nothing a registry could put in a
// digest string can introduce a path separator.
func shortDigest(digest string) string {
	hex := digest[strings.LastIndex(digest, ":")+1:]
	var short strings.Builder
	for _, r := range hex {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			short.WriteRune(r)
		}
		if short.Len() == shortDigestLen {
			break
		}
	}
	if short.Len() == 0 {
		return noDigest
	}
	return short.String()
}

// write marshals the payload and lands it atomically: temp file in the same
// directory, then rename. A crash mid-write leaves a *.tmp file that the
// sweep collects; readers never observe a partial payload.
func (s *Spool) write(name string, payload any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding payload %s: %w", name, err)
	}
	data = append(data, '\n')
	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing payload %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, name)); err != nil {
		return fmt.Errorf("publishing payload %s: %w", name, err)
	}
	return nil
}

// sweep deletes payloads older than the maximum age, and any temp files a
// crash left behind.
func (s *Spool) sweep(now time.Time) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("listing spool: %w", err)
	}
	cutoff := now.Add(-s.maxAge)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue // raced with a rename or delete — the next sweep sees the truth
		}
		if info.ModTime().Before(cutoff) || (strings.HasSuffix(entry.Name(), ".tmp") && info.ModTime().Before(now.Add(-time.Minute))) {
			if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("sweeping spool: %w", err)
			}
		}
	}
	return nil
}
