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
	"sort"
	"strings"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/inventory"
	"github.com/RebuildStackCo/runtime-agent/internal/journal"
	"github.com/RebuildStackCo/runtime-agent/internal/metadata"
	"github.com/RebuildStackCo/runtime-agent/internal/revisions"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// DefaultMaxAge caps how long an unacknowledged payload may sit in the
// spool. Past it, files are removed even without acknowledgment: an
// extended outage degrades to memory-only behavior instead of filling the
// volume (ADR 0007).
const DefaultMaxAge = 24 * time.Hour

// The spool's hard bounds, enforced oldest-first after the age cutoff
// (ADR 0042). Age alone bounds nothing an adversary controls: the number of
// payloads is driven by cluster events, and a pod restarting in a tight loop
// produces a file per restart. Without these the spool grows until the node's
// ephemeral storage is gone, at which point the kubelet starts evicting pods —
// other people's pods, on a node this agent is a guest of.
//
// Both are constants rather than values. The spool is always an emptyDir and no
// setting changes that (ADR 0026), so a knob here would be a promise about a
// volume the operator does not choose.
const (
	// DefaultMaxBytes is the total size of payload files the spool will hold.
	// Sized to be comfortably larger than an active cluster's day and
	// comfortably smaller than the emptyDir's own sizeLimit, so the agent's
	// bound is the one that acts and the kubelet's is the backstop.
	DefaultMaxBytes = 512 << 20 // 512 MiB
	// DefaultMaxFiles bounds the count independently, because bytes do not.
	// The smallest payload is a few hundred bytes, so a byte budget alone
	// permits millions of files — enough to exhaust inodes and to make every
	// sweep's directory listing expensive.
	DefaultMaxFiles = 20000
)

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
	dir      string
	maxAge   time.Duration
	maxBytes int64
	maxFiles int
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
	return &Spool{dir: dir, maxAge: maxAge, maxBytes: DefaultMaxBytes, maxFiles: DefaultMaxFiles}, nil
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
// upsert-by-key ingest. There is no ordering field: the spool holds one version
// of a key at a time, so the newest write is the only one there is (ADR 0027).
//
// CapturedAt dates the assembly of the snapshot, not each fact in it: the node
// facts were collected by scans that finished at various moments before it.
// How much of the fleet those scans covered is what Coverage answers.
type goInventoryPayload struct {
	Kind       string               `json:"kind"`
	Source     string               `json:"source"`
	CapturedAt time.Time            `json:"captured_at"`
	Coverage   inventory.Coverage   `json:"coverage"`
	Records    []inventory.GoRecord `json:"records"`
}

// goBuildPayload is what one build is made of and how it was built, keyed by its
// image digest: the toolchain, the dependency module set, and the allow-listed
// build settings. Unlike every other structural payload it neither supersedes
// nor carries a capture time — these are properties of the build, fixed the
// moment the image was produced, so the payload is immutable given its key and a
// redelivery is byte-identical (ADR 0017).
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
	WindowStart   time.Time               `json:"window_start"`
	WindowSeconds int64                   `json:"window_seconds"`
	Records       []journal.RestartRecord `json:"records"`
}

// restartCountersPayload is the restart counter of every collected container
// that has ever restarted, as it stands at this capture (ADR 0034).
//
// It is journal provenance like the restart windows beside it, and the opposite
// shape. A window says what the agent watched happen between two instants; this
// says what the counter reads, including the restarts that happened before the
// agent was watching and therefore belong to no window at all. That is the fact
// a freshly installed agent can state and a journal cannot.
//
// The two must never be added. `restarts` is the total; the windows are a
// subset of it, and `restarts_before_observation` is the part of the total that
// no window will ever hold.
//
// Superseding batch under a fixed key, like the structural snapshots: it
// describes the counter's current value, so the newest reading replaces its
// predecessor rather than accumulating.
type restartCountersPayload struct {
	Kind       string                     `json:"kind"`
	Source     string                     `json:"source"`
	CapturedAt time.Time                  `json:"captured_at"`
	Records    []collector.RestartCounter `json:"records"`
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
	WindowStart   time.Time                  `json:"window_start"`
	WindowSeconds int64                      `json:"window_seconds"`
	Records       []journal.DisruptionRecord `json:"records"`
}

// jobRunsPayload is every finished Job run of one window: one file per window
// holding many records, like the restart and disruption journals (ADR 0029).
//
// The window is a delivery boundary rather than an aggregation — a run finishes
// once and carries its own instants — and it exists to keep the spool's file
// count bounded by time rather than by how many CronJobs the cluster schedules.
//
// It supersedes within its window: a run reported again while its object still
// exists rewrites its own record, so the file's newest write is its value.
type jobRunsPayload struct {
	Kind          string                 `json:"kind"`
	Source        string                 `json:"source"`
	WindowStart   time.Time              `json:"window_start"`
	WindowSeconds int64                  `json:"window_seconds"`
	Records       []journal.JobRunRecord `json:"records"`
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
	CapturedAt time.Time            `json:"captured_at"`
	Nodes      []collector.NodeInfo `json:"nodes"`
}

// deploymentRevisionsPayload is the current revision history of every collected
// Deployment: which ReplicaSets exist, what each runs, and how many replicas
// each is carrying (ADR 0030).
//
// A superseding batch under a fixed key, the same shape as workload metadata and
// for the same reason: it describes current cluster state, so the newest
// snapshot replaces its predecessor rather than accumulating. Kubernetes keeps
// only `revisionHistoryLimit` revisions, so history beyond that is the backend's
// to accumulate across snapshots, exactly as ADR 0018 decided for the Go
// inventory.
//
// Scope is Deployments alone. StatefulSet and DaemonSet revisions live in
// `controllerrevisions`, which the agent has no RBAC for; the kind's name says
// so rather than implying a generality it lacks.
type deploymentRevisionsPayload struct {
	Kind       string             `json:"kind"`
	Source     string             `json:"source"`
	CapturedAt time.Time          `json:"captured_at"`
	Records    []revisions.Record `json:"records"`
}

// workloadPolicyPayload is what bounds each collected workload from outside its
// own spec: the disruption budgets covering it, the autoscalers driving it, and
// the volume claims holding it in place (ADR 0032).
//
// A superseding snapshot under a fixed key. Every fact is on a live object, so
// the snapshot holds no state and is loss-harmless by construction (ADR 0003),
// and a workload nothing constrains contributes no record at all.
type workloadPolicyPayload struct {
	Kind       string    `json:"kind"`
	Source     string    `json:"source"`
	CapturedAt time.Time `json:"captured_at"`
	// UnavailableSources names the caches this payload could not read at this
	// capture — a denied permission, most often. It is what keeps every
	// absence below conditional: without it, "this workload has no budget" and
	// "we were not allowed to look for budgets" are the same silence, and a
	// workload whose only constraint was a budget disappears from `records`
	// entirely rather than appearing with a field missing (ADR 0033).
	//
	// Empty on an ordinary cluster, and empty is itself a statement: every
	// source was read, so what is not here does not exist.
	UnavailableSources []string                   `json:"unavailable_sources,omitempty"`
	Records            []collector.WorkloadPolicy `json:"records"`
}

// clusterPolicyPayload is the policy configuration of the cluster: what each
// collected namespace imposes on the workloads inside it, and the two
// cluster-scoped catalogs a workload's own fields point into by name (ADR 0032).
//
// One payload rather than three because it has one subject — the cluster's
// policy — and the scopes inside it are visible in the structure rather than
// flattened away, which is ADR 0014's nesting principle applied above the pod.
type clusterPolicyPayload struct {
	Kind       string    `json:"kind"`
	Source     string    `json:"source"`
	CapturedAt time.Time `json:"captured_at"`
	// UnavailableSources is this payload's own list, not a copy of the
	// workload payload's: the two have distinct natural keys and are upserted
	// independently, so each must be readable alone (ADR 0033).
	UnavailableSources []string                `json:"unavailable_sources,omitempty"`
	Policy             collector.ClusterPolicy `json:"policy"`
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
// of the backend's supersede-by-key ingest.
//
// It no longer sweeps. Sweeping rode this cadence to avoid a second timer, and
// this function runs only when there are usage records — so on a cluster where
// the kubelet cannot be polled the spool was never bounded at all. Sweep is now
// the agent's own periodic call (ADR 0042).
func (s *Spool) WriteUsageSnapshot(records []*rollup.Record, obs collector.Observation) error {
	for k, group := range groupByWindow(records) {
		payload := usagePayload{
			Kind:          "usage_snapshot",
			Source:        SourceMeasured,
			WindowStart:   k.start,
			WindowSeconds: k.seconds,
			Observation:   obs,
			Records:       group,
		}
		if err := s.write(k.name()+".snapshot.json", payload); err != nil {
			return err
		}
	}
	return nil
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
func (s *Spool) WriteContainerRestarts(records []journal.RestartRecord) error {
	grouped := make(map[windowKey][]journal.RestartRecord)
	for _, r := range records {
		k := windowKey{start: r.WindowStart, seconds: r.WindowSeconds}
		grouped[k] = append(grouped[k], r)
	}
	for k, group := range grouped {
		payload := containerRestartsPayload{
			Kind:          "container_restarts",
			Source:        SourceJournal,
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
func (s *Spool) WritePodDisruptions(records []journal.DisruptionRecord) error {
	grouped := make(map[windowKey][]journal.DisruptionRecord)
	for _, r := range records {
		k := windowKey{start: r.WindowStart, seconds: r.WindowSeconds}
		grouped[k] = append(grouped[k], r)
	}
	for k, group := range grouped {
		payload := podDisruptionsPayload{
			Kind:          "pod_disruptions",
			Source:        SourceJournal,
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
		e.FinishedAt.Unix(), fileToken(e.Namespace), fileToken(e.Pod), fileToken(e.Container), e.RestartCount)
	return s.write(name, oomPayload{Kind: "oom_kill", Source: SourceJournal, Event: e})
}

// WriteRestartCounters writes the current restart-counter reading of every
// collected container as one superseding batch. records must already be in a
// deterministic order (PodWatcher.RestartCounters sorts them) so the payload
// bytes are stable — the golden contract.
//
// A cluster where nothing has ever restarted writes an empty record list rather
// than nothing at all, unlike the journals: the reading is a snapshot, and for a
// snapshot "no container has restarted" is an answer worth stating. A journal
// window with no events says only that the agent was watching, which the open
// usage window already says.
func (s *Spool) WriteRestartCounters(capturedAt time.Time, records []collector.RestartCounter) error {
	if records == nil {
		records = []collector.RestartCounter{}
	}
	payload := restartCountersPayload{
		Kind:       "restart_counters",
		Source:     SourceJournal,
		CapturedAt: capturedAt.UTC(),
		Records:    records,
	}
	return s.write("restart-counters.json", payload)
}

// WriteJobRuns writes the finished Job runs of each window they belong to, one
// file per window, atomically replacing that window's previous file. records
// must already be in a deterministic order (the accumulator sorts them) so the
// payload bytes are stable — the golden contract.
//
// A window in which no Job finished writes nothing. A cluster with no batch
// workloads has nothing to say, and an empty payload would claim otherwise for
// every quiet hour of every cluster.
func (s *Spool) WriteJobRuns(records []journal.JobRunRecord) error {
	grouped := make(map[windowKey][]journal.JobRunRecord)
	for _, r := range records {
		k := windowKey{start: r.WindowStart, seconds: r.WindowSeconds}
		grouped[k] = append(grouped[k], r)
	}
	for k, group := range grouped {
		payload := jobRunsPayload{
			Kind:          "job_runs",
			Source:        SourceJournal,
			WindowStart:   k.start,
			WindowSeconds: k.seconds,
			Records:       group,
		}
		if err := s.write(fmt.Sprintf("job-runs-%d-%d.json", k.start.Unix(), k.seconds), payload); err != nil {
			return err
		}
	}
	return nil
}

// WriteGoInventory writes the current Go inventory as one superseding batch
// (ADR 0010). One file per cluster, atomically replaced each flush: the newest
// inventory supersedes its predecessor, exactly as an open-window usage
// snapshot does. records must already be in a deterministic order (the store
// sorts them) so the payload bytes are stable — the golden contract. capturedAt
// is passed in rather than read from the clock for the same reason.
func (s *Spool) WriteGoInventory(capturedAt time.Time, cov inventory.Coverage, records []inventory.GoRecord) error {
	payload := goInventoryPayload{
		Kind:       "go_inventory",
		Source:     SourceStructural,
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
func (s *Spool) WriteWorkloadMetadata(capturedAt time.Time, records []metadata.Record) error {
	payload := workloadMetadataPayload{
		Kind:       "workload_metadata",
		Source:     SourceStructural,
		CapturedAt: capturedAt.UTC(),
		Records:    records,
	}
	return s.write("workload-metadata.json", payload)
}

// WriteNodeMetadata writes the current node inventory as one superseding batch.
// nodes must already be sorted by name (NodeWatcher.Nodes does it).
func (s *Spool) WriteNodeMetadata(capturedAt time.Time, nodes []collector.NodeInfo) error {
	payload := nodeMetadataPayload{
		Kind:       "node_metadata",
		Source:     SourceStructural,
		CapturedAt: capturedAt.UTC(),
		Nodes:      nodes,
	}
	return s.write("node-metadata.json", payload)
}

// WriteDeploymentRevisions writes the current revision history as one
// superseding batch. records must already be in a deterministic order
// (revisions.Aggregate sorts them) so the payload bytes are stable — the golden
// contract.
func (s *Spool) WriteDeploymentRevisions(capturedAt time.Time, records []revisions.Record) error {
	payload := deploymentRevisionsPayload{
		Kind:       "deployment_revisions",
		Source:     SourceStructural,
		CapturedAt: capturedAt.UTC(),
		Records:    records,
	}
	return s.write("deployment-revisions.json", payload)
}

// WriteWorkloadPolicy writes the workload-policy snapshot. It supersedes its
// predecessor under a fixed name, like the other structural snapshots.
func (s *Spool) WriteWorkloadPolicy(capturedAt time.Time, records []collector.WorkloadPolicy, unavailable []string) error {
	payload := workloadPolicyPayload{
		Kind:               "workload_policy",
		Source:             SourceStructural,
		CapturedAt:         capturedAt.UTC(),
		UnavailableSources: unavailable,
		Records:            records,
	}
	return s.write("workload-policy.json", payload)
}

// WriteClusterPolicy writes the cluster-policy snapshot.
func (s *Spool) WriteClusterPolicy(capturedAt time.Time, policy collector.ClusterPolicy, unavailable []string) error {
	payload := clusterPolicyPayload{
		Kind:               "cluster_policy",
		Source:             SourceStructural,
		CapturedAt:         capturedAt.UTC(),
		UnavailableSources: unavailable,
		Policy:             policy,
	}
	return s.write("cluster-policy.json", payload)
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
		fileToken(key.Namespace), fileToken(key.Workload), fileToken(key.Container),
		shortDigest(key.ImageDigest), key.CaptureStart.Unix(), key.CaptureEnd.Unix())
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

// fileTokenMax bounds one filename component. Names are assembled from several,
// and a filesystem's own limit is on the whole name, so each has to be bounded
// for the total to be. Sixty-three is the DNS label length, which is what most
// of these components already are.
const fileTokenMax = 63

// unnamed stands in for a component that sanitizes to nothing. A literal keeps
// a directory listing legible where an empty segment would read as a bug.
const unnamed = "unnamed"

// fileToken reduces a caller-supplied string to something safe to put in a
// filename (ADR 0042).
//
// The one that made this necessary is the workload name, which comes from
// `ownerReferences[].name` — and the API server validates that field only for
// non-emptiness, no DNS-1123, no character set. So a pod created with an owner
// reference named "x/../../../../etc/cron.d/evil" put that straight into a path
// that filepath.Join then resolved. The namespace, pod and container names
// beside it happen to be DNS-1123 and were safe by luck rather than by
// anything this package did.
//
// Rather than sanitize the one field that is known to be attacker-controlled,
// every component goes through here: which Kubernetes fields are validated is
// not this package's business to track.
func fileToken(s string) string {
	var b strings.Builder
	allDots := true
	for _, r := range s {
		switch {
		case (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '-' || r == '_':
			allDots = false
			b.WriteRune(r)
		case r == '.':
			b.WriteRune(r)
		default:
			allDots = false
			b.WriteRune('-')
		}
		if b.Len() >= fileTokenMax {
			break
		}
	}
	// "." and ".." are directories, not names: a component of only dots would
	// make filepath.Join climb rather than descend.
	if b.Len() == 0 || allDots {
		return unnamed
	}
	return b.String()
}

// write marshals the payload and lands it atomically: temp file in the same
// directory, then rename. A crash mid-write leaves a *.tmp file that the
// sweep collects; readers never observe a partial payload.
//
// It also refuses a name that is not a plain filename. Callers already build
// their names from fileToken, so this catches nothing today — it is here for
// the next caller, because every path this package writes goes through this one
// function and that makes it the only place worth checking (ADR 0042).
func (s *Spool) write(name string, payload any) error {
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("refusing to write payload under %q: a spool payload name is a plain filename", name)
	}
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

// Sweep enforces the spool's bounds: payloads older than the maximum age go,
// temp files a crash left behind go, and whatever still exceeds the size or
// count budget goes oldest-first.
//
// It is exported and called on the agent's own cadence (ADR 0042). It used to
// be private and called from WriteUsageSnapshot, which rode the snapshot
// cadence "so no extra timer exists" — a saving that quietly made the whole
// bound conditional. WriteUsageSnapshot runs only when there are usage records
// to write, so on a cluster where the kubelet cannot be polled — the
// nodes/proxy grant withheld, or every kubelet unreachable — nothing swept at
// all, and the spool grew without any limit while the agent looked healthy.
//
// Oldest-first is the right loss. The oldest payload is the one most likely to
// have been superseded, and every payload here is loss-harmless by design
// (ADR 0007): what the spool holds is a convenience, never a fact that exists
// nowhere else.
func (s *Spool) Sweep(now time.Time) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("listing spool: %w", err)
	}

	type file struct {
		name    string
		size    int64
		modTime time.Time
	}
	var kept []file
	var total int64
	cutoff := now.Add(-s.maxAge)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue // raced with a rename or delete — the next sweep sees the truth
		}
		expired := info.ModTime().Before(cutoff)
		orphanTmp := strings.HasSuffix(entry.Name(), ".tmp") && info.ModTime().Before(now.Add(-time.Minute))
		if expired || orphanTmp {
			if err := s.remove(entry.Name()); err != nil {
				return err
			}
			continue
		}
		kept = append(kept, file{name: entry.Name(), size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}

	if total <= s.maxBytes && len(kept) <= s.maxFiles {
		return nil
	}

	// Oldest first, and by name where the timestamps tie so the order is
	// deterministic — a directory listing's order is not.
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].modTime.Equal(kept[j].modTime) {
			return kept[i].name < kept[j].name
		}
		return kept[i].modTime.Before(kept[j].modTime)
	})
	count := len(kept)
	for _, f := range kept {
		if total <= s.maxBytes && count <= s.maxFiles {
			break
		}
		if err := s.remove(f.name); err != nil {
			return err
		}
		total -= f.size
		count--
	}
	return nil
}

func (s *Spool) remove(name string) error {
	if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sweeping spool: %w", err)
	}
	return nil
}
