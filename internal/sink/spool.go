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

	"github.com/RebuildStackCo/runtime-agent/internal/config"
	"github.com/RebuildStackCo/runtime-agent/internal/inventory"
	"github.com/RebuildStackCo/runtime-agent/internal/journal"
	"github.com/RebuildStackCo/runtime-agent/internal/metadata"
	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofprobe"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofpull"
	"github.com/RebuildStackCo/runtime-agent/internal/revisions"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// DefaultMaxAge caps how long an unacknowledged payload may sit in the
// spool. Past it, files are removed even without acknowledgment: an
// extended outage degrades to memory-only behavior instead of filling the
// volume (ADR 0007).
const DefaultMaxAge = 24 * time.Hour

// The spool's hard bounds, enforced oldest-first after the age cutoff
// (ADR 0042). Age alone bounds nothing an adversary controls — a pod restarting
// in a tight loop produces a file per restart — and an unbounded spool ends with
// the kubelet evicting other people's pods off the node.
//
// Constants rather than values: the spool is always an emptyDir (ADR 0026), so a
// knob here would be a promise about a volume the operator does not choose.
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
	// SourceAgent is a fact about the agent's own collection rather than about
	// the cluster: what it looked at, what it was configured to skip, and which
	// of its reads worked. The other four classes all answer "where in the
	// cluster did this come from", and stretching one of them over a fact whose
	// subject is the agent would make the discriminator lie in the one payload
	// whose whole purpose is to be trusted about itself (ADR 0054 §1).
	SourceAgent = "agent"
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
	Kind          string            `json:"kind"`
	Source        string            `json:"source"`
	WindowStart   time.Time         `json:"window_start"`
	WindowSeconds int64             `json:"window_seconds"`
	Observation   model.Observation `json:"observation"`
	Records       []*rollup.Record  `json:"records"`
}

// networkPayload is one closed window of every collected workload's network
// counters. Pod-scoped, so its records key on the workload and not the
// container the usage records key on (ADR 0053 §1).
//
// It carries the same Observation as the usage payloads and for the same
// reason: these are measured facts, and a workload that moved nothing looks
// exactly like one whose node could not be scraped (ADR 0012).
type networkPayload struct {
	Kind          string                  `json:"kind"`
	Source        string                  `json:"source"`
	WindowStart   time.Time               `json:"window_start"`
	WindowSeconds int64                   `json:"window_seconds"`
	Observation   model.Observation       `json:"observation"`
	Records       []*rollup.NetworkRecord `json:"records"`
}

// collectionCoveragePayload is what the agent did, not what it found: what it
// saw, what it was told to skip and under which control, which of its reads
// worked, and the shape of the configuration in force.
//
// It ships on every flush whether or not anything changed, so an old CapturedAt
// says the agent stopped where silence in every other kind says nothing
// (ADR 0054 §5). Every number here is an aggregate; no name of an excluded
// object appears (CLAUDE.md invariant 6).
type collectionCoveragePayload struct {
	Kind       string    `json:"kind"`
	Source     string    `json:"source"`
	CapturedAt time.Time `json:"captured_at"`
	// Since is the base of every cumulative counter below: the moment this
	// process started counting. It travels in the bytes so a restart reads as a
	// new base rather than as counters that fell.
	Since   time.Time            `json:"since"`
	Agent   AgentInfo            `json:"agent"`
	Sources []model.SourceHealth `json:"sources"`
	Filter  model.Coverage       `json:"filter"`
	// Placement counts the placement terms and values the reduction refused to
	// carry (ADR 0031). Not an exclusion — the workload is collected — but it is
	// something the customer's manifests hold and the payload does not.
	Placement model.PlacementDrops `json:"placement"`
	// Nodes counts what the node reductions refused to carry: a condition type
	// or device vendor outside its allow-list, a taint whose strings did not
	// fit (ADR 0064 §4). Same class as Placement above — the node is collected,
	// but part of what it says is not.
	Nodes model.NodeDrops `json:"nodes"`
	// Inventory and Scan are present only when the node role is deployed.
	Inventory *inventory.Counters     `json:"inventory,omitempty"`
	Scan      *inventory.ScanCoverage `json:"scan,omitempty"`
	// EBPF is present once a node has reported what its profiler did. Unlike
	// Scan beside it, its counts are cumulative since each node started rather
	// than one pass, and `states` says why a fleet is not profiling at all
	// (ADR 0060 §3).
	EBPF *inventory.ProfileCoverage `json:"ebpf,omitempty"`
	// Pprof is present only when endpoint discovery is on. It counts targets by
	// their latest answer, which is what makes "no profiles for this workload"
	// legible as one of three different things (ADR 0057 §5).
	Pprof *pprofprobe.Coverage `json:"pprof,omitempty"`
	// PprofPull is present only when profiles are pulled. `refused` is the one
	// to read first: it is a workload whose own profiler holds the single CPU
	// profile Go allows, not a workload the agent failed on (ADR 0058 §3).
	PprofPull *pprofpull.Coverage `json:"pprof_pull,omitempty"`
}

// AgentInfo is what the agent is and how it is set up, for a report that needs
// to state what its findings rest on.
type AgentInfo struct {
	// Version is the build. "dev" means an unstamped binary.
	Version string `json:"version"`
	// Config is the shape of the configuration in force — counts and switches,
	// never a name from it (ADR 0054 §2).
	Config config.Shape `json:"config"`
	// UsageSignals is which kubelet signals this cluster actually exposes. A
	// finding resting on PSI cannot be made on a cluster that reports none.
	UsageSignals []string `json:"usage_signals,omitempty"`
}

// oomPayload is one OOM kill event; events bypass windows and ship
// immediately (ADR 0006).
type oomPayload struct {
	Kind   string        `json:"kind"`
	Source string        `json:"source"`
	Event  model.OOMKill `json:"event"`
}

// goInventoryPayload is the current Go inventory of the cluster — one record per
// (namespace, workload, container), joined from node facts (ADR 0010). A single
// superseding batch under a fixed key, with no ordering field: the spool holds
// one version of a key at a time (ADR 0027).
//
// CapturedAt dates the assembly, not each fact in it; how much of the fleet the
// underlying scans covered is what Coverage answers.
type goInventoryPayload struct {
	Kind       string               `json:"kind"`
	Source     string               `json:"source"`
	CapturedAt time.Time            `json:"captured_at"`
	Coverage   inventory.Coverage   `json:"coverage"`
	Records    []inventory.GoRecord `json:"records"`
}

// processPeaksPayload is what the processes of each collected workload container
// currently reach on the nodes that run them: a superseding batch under a fixed
// key, like the inventory beside it (ADR 0052).
//
// CapturedAt dates the assembly. The peaks themselves carry no window — each is
// a high-water mark since its process started, which is a reading rather than a
// measurement over an interval (ADR 0034's shape).
type processPeaksPayload struct {
	Kind       string                 `json:"kind"`
	Source     string                 `json:"source"`
	CapturedAt time.Time              `json:"captured_at"`
	Records    []inventory.PeakRecord `json:"records"`
}

// listeningPortsPayload is where each collected workload container accepts
// connections, as its processes report it: a superseding batch under a fixed
// key, like the peaks beside it.
//
// It is the pod's own view, read from the processes' descriptors rather than
// from the spec — so a port nobody declared is here, and a declared port nothing
// binds is not (ADR 0056 §2).
type listeningPortsPayload struct {
	Kind       string                 `json:"kind"`
	Source     string                 `json:"source"`
	CapturedAt time.Time              `json:"captured_at"`
	Records    []inventory.PortRecord `json:"records"`
}

// processCountersPayload is what each collected workload container's processes
// did over the most recent interval each node measured: a superseding batch
// under a fixed key, like the peaks and ports beside it (ADR 0062).
//
// CapturedAt dates the assembly, and it is not the window. Each record carries
// its own `observed_nanos`, which is the process-time the deltas cover — the
// denominator of a rate, and the only thing that makes the sums comparable
// between two flushes.
type processCountersPayload struct {
	Kind       string                    `json:"kind"`
	Source     string                    `json:"source"`
	CapturedAt time.Time                 `json:"captured_at"`
	Records    []inventory.CounterRecord `json:"records"`
}

// ProfileDrops is how much of a pulled profile the allow-list removed. It is the
// count and never the identity: which functions were redacted is the thing the
// filter exists to keep inside the cluster (CLAUDE.md invariant 6).
type ProfileDrops struct {
	// ThirdPartyFrames and UnsymbolizedFrames are frames replaced by the
	// neutral placeholder; SamplesTouched is how many stacks lost at least one.
	ThirdPartyFrames   uint64 `json:"third_party_frames"`
	UnsymbolizedFrames uint64 `json:"unsymbolized_frames"`
	SamplesTouched     uint64 `json:"samples_touched"`
}

// pulledProfilePayload is one CPU profile fetched from a workload's own
// endpoint: allow-list-filtered pprof bytes (gzipped protobuf, base64 in JSON)
// and the key of the workload and window they belong to.
//
// It accumulates rather than supersedes, like the eBPF kind (ADR 0011 §6). What
// is not here matters as much: no mapping, so no path or build ID of the
// executable, and no sample label — those are strings the service chose
// (ADR 0058 §4).
type pulledProfilePayload struct {
	Kind         string       `json:"kind"`
	Namespace    string       `json:"namespace"`
	Workload     string       `json:"workload"`
	Container    string       `json:"container"`
	ImageDigest  string       `json:"image_digest,omitempty"`
	CaptureStart time.Time    `json:"capture_start"`
	CaptureEnd   time.Time    `json:"capture_end"`
	Source       string       `json:"source"`
	Dropped      ProfileDrops `json:"dropped"`
	Pprof        []byte       `json:"pprof"`
}

// goBuildPayload is what one build is made of and how it was built, keyed by its
// image digest. Alone among the structural payloads it neither supersedes nor
// carries a capture time — these are properties of the build, fixed when the
// image was produced, so a redelivery is byte-identical (ADR 0017).
//
// Settings may be absent: the toolchain records vcs.* only when it can, which in
// container builds is the exception (ADR 0019). GoDebug may be absent for a
// second reason — the build matches its toolchain's own defaults (ADR 0050 §2).
type goBuildPayload struct {
	Kind        string            `json:"kind"`
	Source      string            `json:"source"`
	ImageDigest string            `json:"image_digest"`
	GoVersion   string            `json:"go_version"`
	MainModule  string            `json:"main_module"`
	Modules     []nodescan.Module `json:"modules"`
	Settings    map[string]string `json:"settings,omitempty"`
	GoDebug     map[string]string `json:"godebug,omitempty"`
	// PprofEndpoint says the build links net/http/pprof, so a `/debug/pprof`
	// handler exists in it. Whether anything serves that handler, and on which
	// port, is not a property of the build and is not claimed here (ADR 0056 §1).
	PprofEndpoint bool `json:"pprof_endpoint,omitempty"`
}

// containerRestartsPayload is every collected container's restart history within
// one window: one file per window holding many records (ADR 0020).
//
// Grouping by window is the decision the shape encodes — a payload per restart
// would put the spool's file count under the control of a crash loop — and the
// question a restart answers ("how often, and why") is a property of the window.
// It supersedes within its window, and there is no separate closed-window kind,
// because a restart count only grows.
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
// Journal provenance like the windows beside it, and the opposite shape: a
// window says what the agent watched happen, this says what the counter reads,
// including restarts that belong to no window at all. The two must never be
// added — the windows are a subset of `restarts`. Superseding batch under a
// fixed key: the newest reading replaces its predecessor.
type restartCountersPayload struct {
	Kind       string                 `json:"kind"`
	Source     string                 `json:"source"`
	CapturedAt time.Time              `json:"captured_at"`
	Records    []model.RestartCounter `json:"records"`
}

// podDisruptionsPayload is every pod the cluster removed within one window:
// preempted, evicted under node pressure, drained, or deleted through the
// eviction API (ADR 0021). One file per window holding many records.
//
// The window is a delivery boundary rather than an aggregation — a pod is
// disrupted once and each record carries its own instant — and it keeps the
// spool's file count bounded by time rather than by how bad an incident is.
type podDisruptionsPayload struct {
	Kind          string                     `json:"kind"`
	Source        string                     `json:"source"`
	WindowStart   time.Time                  `json:"window_start"`
	WindowSeconds int64                      `json:"window_seconds"`
	Records       []journal.DisruptionRecord `json:"records"`
}

// nodeLifecyclePayload is every node that joined or left the cluster within one
// window (ADR 0064 §3). One file per window, a delivery boundary rather than an
// aggregation, exactly like the disruption journal above.
type nodeLifecyclePayload struct {
	Kind          string                    `json:"kind"`
	Source        string                    `json:"source"`
	WindowStart   time.Time                 `json:"window_start"`
	WindowSeconds int64                     `json:"window_seconds"`
	Records       []journal.NodeEventRecord `json:"records"`
}

// jobRunsPayload is every finished Job run of one window: one file per window
// holding many records, like the restart and disruption journals (ADR 0029).
//
// The window is a delivery boundary, not an aggregation, and keeps the file
// count bounded by time rather than by how many CronJobs the cluster schedules.
// It supersedes within its window: a run reported again rewrites its own record.
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
	Kind       string           `json:"kind"`
	Source     string           `json:"source"`
	CapturedAt time.Time        `json:"captured_at"`
	Nodes      []model.NodeInfo `json:"nodes"`
}

// workloadRevisionsPayload is the current revision history of every collected
// workload that manages ReplicaSets: which sets exist, what each runs, how many
// replicas each carries (ADR 0030, ADR 0049).
//
// Superseding batch under a fixed key. Kubernetes keeps only
// `revisionHistoryLimit` revisions, so history beyond that is the backend's to
// accumulate (ADR 0018). StatefulSet and DaemonSet revisions live in
// `controllerrevisions`, which the agent does not read.
type workloadRevisionsPayload struct {
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
	// capture. It is what keeps every absence below conditional: without it,
	// "this workload has no budget" and "we were not allowed to look" are the
	// same silence (ADR 0033). Empty is itself a statement — every source was
	// read, so what is not here does not exist.
	UnavailableSources []string               `json:"unavailable_sources,omitempty"`
	Records            []model.WorkloadPolicy `json:"records"`
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
	UnavailableSources []string            `json:"unavailable_sources,omitempty"`
	Policy             model.ClusterPolicy `json:"policy"`
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
func (s *Spool) WriteUsageSnapshot(records []*rollup.Record, obs model.Observation) error {
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
func (s *Spool) WriteClosedWindows(records []*rollup.Record, obs model.Observation) error {
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

// WriteNetworkWindows writes final closed-window network records, one file per
// window. There is no snapshot to remove: this kind has none.
func (s *Spool) WriteNetworkWindows(records []*rollup.NetworkRecord, obs model.Observation) error {
	grouped := make(map[windowKey][]*rollup.NetworkRecord)
	for _, r := range records {
		grouped[windowKey{start: r.WindowStart, seconds: r.WindowSeconds}] = append(
			grouped[windowKey{start: r.WindowStart, seconds: r.WindowSeconds}], r)
	}
	for k, group := range grouped {
		payload := networkPayload{
			Kind:          "network_window",
			Source:        SourceMeasured,
			WindowStart:   k.start,
			WindowSeconds: k.seconds,
			Observation:   obs,
			Records:       group,
		}
		if err := s.write(fmt.Sprintf("network-%d-%d.json", k.start.Unix(), k.seconds), payload); err != nil {
			return err
		}
	}
	return nil
}

// WriteCollectionCoverage writes the coverage payload, superseding its
// predecessor. It is written on every flush, including one that found nothing:
// an empty report and a broken agent are the same bytes without it (ADR 0054).
func (s *Spool) WriteCollectionCoverage(capturedAt, since time.Time, agent AgentInfo, sources []model.SourceHealth, filter model.Coverage, placement model.PlacementDrops, nodes model.NodeDrops, inv *inventory.Counters, scan *inventory.ScanCoverage, ebpf *inventory.ProfileCoverage, probe *pprofprobe.Coverage, pull *pprofpull.Coverage) error {
	payload := collectionCoveragePayload{
		Kind:       "collection_coverage",
		Source:     SourceAgent,
		CapturedAt: capturedAt.UTC(),
		Since:      since.UTC(),
		Agent:      agent,
		Sources:    sources,
		Filter:     filter,
		Placement:  placement,
		Nodes:      nodes,
		Inventory:  inv,
		Scan:       scan,
		EBPF:       ebpf,
		Pprof:      probe,
		PprofPull:  pull,
	}
	return s.write("collection-coverage.json", payload)
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

// WriteNodeLifecycle writes the node arrivals and departures of each window they
// belong to, one file per window, atomically replacing that window's previous
// file. records must already be in a deterministic order (the accumulator sorts
// them) so the payload bytes are stable — the golden contract.
//
// A window in which the fleet did not change writes nothing, like the other
// journals: a stable fleet has nothing to report, and `node_metadata` already
// says what it currently is.
func (s *Spool) WriteNodeLifecycle(records []journal.NodeEventRecord) error {
	grouped := make(map[windowKey][]journal.NodeEventRecord)
	for _, r := range records {
		k := windowKey{start: r.WindowStart, seconds: r.WindowSeconds}
		grouped[k] = append(grouped[k], r)
	}
	for k, group := range grouped {
		payload := nodeLifecyclePayload{
			Kind:          "node_lifecycle",
			Source:        SourceJournal,
			WindowStart:   k.start,
			WindowSeconds: k.seconds,
			Records:       group,
		}
		if err := s.write(fmt.Sprintf("node-lifecycle-%d-%d.json", k.start.Unix(), k.seconds), payload); err != nil {
			return err
		}
	}
	return nil
}

// WriteOOMKill writes one OOM event immediately. The filename carries the
// event's natural identity, so a re-observed event overwrites rather than
// duplicates.
func (s *Spool) WriteOOMKill(e model.OOMKill) error {
	name := fmt.Sprintf("oom-%d-%s-%s-%s-%d.json",
		e.FinishedAt.Unix(), fileToken(e.Namespace), fileToken(e.Pod), fileToken(e.Container), e.RestartCount)
	return s.write(name, oomPayload{Kind: "oom_kill", Source: SourceJournal, Event: e})
}

// WriteRestartCounters writes the current restart-counter reading of every
// collected container as one superseding batch. records must already be sorted
// (PodWatcher.RestartCounters does it) so the payload bytes are stable.
//
// A cluster where nothing has ever restarted writes an empty record list rather
// than nothing, unlike the journals: for a snapshot, "no container has
// restarted" is an answer worth stating.
func (s *Spool) WriteRestartCounters(capturedAt time.Time, records []model.RestartCounter) error {
	if records == nil {
		records = []model.RestartCounter{}
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

// WriteProcessPeaks writes the current peak records as one superseding batch.
// Records must arrive sorted (Store.PeakSnapshot sorts them) so the payload
// bytes are stable — the golden contract.
func (s *Spool) WriteProcessPeaks(capturedAt time.Time, records []inventory.PeakRecord) error {
	payload := processPeaksPayload{
		Kind:       "process_peaks",
		Source:     SourceMeasured,
		CapturedAt: capturedAt.UTC(),
		Records:    records,
	}
	return s.write("process-peaks.json", payload)
}

// WriteProcessCounters writes the current counter records as one superseding
// batch. Records must arrive sorted (Store.CounterSnapshot sorts them) so the
// payload bytes are stable — the golden contract.
//
// A cluster where no node has produced a second reading yet writes an empty
// record list rather than nothing: "every process was seen for the first time"
// is an answer, and it is the one that explains an empty first flush.
func (s *Spool) WriteProcessCounters(capturedAt time.Time, records []inventory.CounterRecord) error {
	payload := processCountersPayload{
		Kind:       "process_counters",
		Source:     SourceMeasured,
		CapturedAt: capturedAt.UTC(),
		Records:    records,
	}
	return s.write("process-counters.json", payload)
}

// WriteListeningPorts writes where every collected workload container accepts
// connections, as of capturedAt. One file, superseding, for the reason
// process-peaks is one file: the batch is the answer, and a record per container
// would put the spool's file count under the cluster's control.
func (s *Spool) WriteListeningPorts(capturedAt time.Time, records []inventory.PortRecord) error {
	payload := listeningPortsPayload{
		Kind:       "listening_ports",
		Source:     SourceStructural,
		CapturedAt: capturedAt.UTC(),
		Records:    records,
	}
	return s.write("listening-ports.json", payload)
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
		modules = []nodescan.Module{}
	}
	payload := goBuildPayload{
		Kind:          "go_build",
		Source:        SourceStructural,
		ImageDigest:   b.ImageDigest,
		GoVersion:     b.GoVersion,
		MainModule:    b.MainModule,
		Modules:       modules,
		Settings:      b.Settings,
		GoDebug:       b.GoDebug,
		PprofEndpoint: b.HasPprof,
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
func (s *Spool) WriteNodeMetadata(capturedAt time.Time, nodes []model.NodeInfo) error {
	payload := nodeMetadataPayload{
		Kind:       "node_metadata",
		Source:     SourceStructural,
		CapturedAt: capturedAt.UTC(),
		Nodes:      nodes,
	}
	return s.write("node-metadata.json", payload)
}

// WriteWorkloadRevisions writes the current revision history as one
// superseding batch. records must already be in a deterministic order
// (revisions.Aggregate sorts them) so the payload bytes are stable — the golden
// contract.
func (s *Spool) WriteWorkloadRevisions(capturedAt time.Time, records []revisions.Record) error {
	payload := workloadRevisionsPayload{
		Kind:       "workload_revisions",
		Source:     SourceStructural,
		CapturedAt: capturedAt.UTC(),
		Records:    records,
	}
	return s.write("workload-revisions.json", payload)
}

// WriteWorkloadPolicy writes the workload-policy snapshot. It supersedes its
// predecessor under a fixed name, like the other structural snapshots.
func (s *Spool) WriteWorkloadPolicy(capturedAt time.Time, records []model.WorkloadPolicy, unavailable []string) error {
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
func (s *Spool) WriteClusterPolicy(capturedAt time.Time, policy model.ClusterPolicy, unavailable []string) error {
	payload := clusterPolicyPayload{
		Kind:               "cluster_policy",
		Source:             SourceStructural,
		CapturedAt:         capturedAt.UTC(),
		UnavailableSources: unavailable,
		Policy:             policy,
	}
	return s.write("cluster-policy.json", payload)
}

// WriteProfile writes one captured eBPF CPU profile. Each capture is its own
// file, bounded by the age sweep rather than overwritten (ADR 0011 §6); the
// pprof bytes must already be filtered and validated (ADR 0011 §4–5).
//
// The filename carries the whole natural key, image digest included: without it
// two captures of one container in one window — two replicas on a node, or two
// builds during a rollout — collide and the second replaces the first (ADR 0023).
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

// WritePulledProfile writes one profile fetched from a workload's own
// `/debug/pprof` endpoint. Same shape and same accumulate-by-window discipline
// as the eBPF kind beside it, and a separate kind because the capture is a
// different claim: this one is the workload's own runtime sampling itself, at
// its own rate, for a window the agent asked for (ADR 0058).
func (s *Spool) WritePulledProfile(key ProfileKey, pprof []byte, dropped ProfileDrops) error {
	payload := pulledProfilePayload{
		Kind:         "pprof_profile",
		Namespace:    key.Namespace,
		Workload:     key.Workload,
		Container:    key.Container,
		ImageDigest:  key.ImageDigest,
		CaptureStart: key.CaptureStart,
		CaptureEnd:   key.CaptureEnd,
		Source:       SourceSampled,
		Dropped:      dropped,
		Pprof:        pprof,
	}
	name := fmt.Sprintf("pprof-%s-%s-%s-%s-%d-%d.json",
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
// The field that made it necessary is the workload name, taken from
// `ownerReferences[].name`, which the API server validates only for
// non-emptiness — so "x/../../../../etc/cron.d/evil" went straight into a path
// filepath.Join then resolved. Every component goes through here rather than
// that one: which fields Kubernetes validates is not this package's to track.
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
// Exported and called on the agent's own cadence, because riding the usage
// snapshot made the bound conditional on the kubelet being pollable (ADR 0042).
// Oldest-first is the right loss: the oldest payload is the likeliest to have
// been superseded, and every payload here is loss-harmless (ADR 0007).
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
