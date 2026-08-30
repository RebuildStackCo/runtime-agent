package sink

import "sort"

// The payload registry: the one place that says what this agent ships.
//
// It sits next to the writers and is checked against the golden payload bytes in
// both directions (registry_test.go) — a kind that ships without a row fails, a
// row with no shipped payload fails. It was a table in ADR 0012 and drifted in
// three rows across six later decisions, which is why ADR 0022 moved it here: a
// document cannot fail. Adding a kind means a row, a writer, a golden, an ADR.

// Delivery is how a kind's payloads relate to one another under their natural
// key. It is the discipline the backend's ingest must implement; getting it
// wrong turns an upsert into a duplicate or a duplicate into an overwrite.
type Delivery string

const (
	// DeliverySupersedes means a newer payload replaces the older one under the
	// same key. The backend upserts, last write wins, and no payload carries an
	// ordering field: the spool holds exactly one version of a key, atomically
	// replaced, so the agent never offers two versions to order (ADR 0027).
	DeliverySupersedes Delivery = "supersedes"
	// DeliveryAccumulates means every payload has a distinct key and none
	// replaces another. The backend inserts.
	DeliveryAccumulates Delivery = "accumulates"
	// DeliveryWriteOnce accumulates, and the agent additionally writes any given
	// key at most once: the facts are properties of an immutable artifact, so a
	// redelivery after a restart is byte-identical (ADR 0017).
	DeliveryWriteOnce Delivery = "write-once"
)

// PayloadKind is one row of the registry: what a kind is keyed by, how its
// payloads relate under that key, what class of claim its facts are, and which
// decision put it here.
type PayloadKind struct {
	// Kind is the discriminator in the payload's "kind" field.
	Kind string
	// Source is the provenance class in the payload's "source" field. The
	// backend must never merge two classes under one key (ADR 0012 §2).
	Source string
	// NaturalKey is the tuple the backend upserts on, in prose. It is also what
	// the spool filename encodes, so two payloads of one key are one file
	// (ADR 0003).
	NaturalKey string
	// Delivery is how payloads of this kind relate under that key.
	Delivery Delivery
	// ADR names the decision that introduced or last changed this row, so a
	// reader of the registry can reach the reasoning without searching.
	ADR string
}

// registry is the fixed list. Order here is irrelevant — Registry sorts.
var registry = []PayloadKind{
	{
		// Written on every flush, including one with nothing to report: it is
		// the only kind whose absence or staleness is a fact about the agent
		// rather than about the cluster (ADR 0054).
		Kind:       "collection_coverage",
		Source:     SourceAgent,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0054",
	},
	{
		Kind:       "usage_snapshot",
		Source:     SourceMeasured,
		NaturalKey: "(window start, window length)",
		Delivery:   DeliverySupersedes,
		ADR:        "0006, 0013",
	},
	{
		Kind:       "usage_window",
		Source:     SourceMeasured,
		NaturalKey: "(window start, window length)",
		Delivery:   DeliverySupersedes,
		ADR:        "0006, 0013",
	},
	{
		// Closed windows only: bytes accumulate, so a partial window read as a
		// rate over the whole one is wrong, and nothing in the agent consumes an
		// open network window. So this kind has no snapshot sibling, unlike
		// usage (ADR 0053 §4).
		Kind:       "network_window",
		Source:     SourceMeasured,
		NaturalKey: "(window start, window length)",
		Delivery:   DeliverySupersedes,
		ADR:        "0053",
	},
	{
		Kind:       "oom_kill",
		Source:     SourceJournal,
		NaturalKey: "(finished-at, namespace, pod, container, restart count)",
		Delivery:   DeliveryAccumulates,
		ADR:        "0006, 0013",
	},
	{
		Kind:       "container_restarts",
		Source:     SourceJournal,
		NaturalKey: "(window start, window length)",
		Delivery:   DeliverySupersedes,
		ADR:        "0020",
	},
	{
		// The counter as the kubelet keeps it, not the advances the agent
		// watched. It is the only payload that carries restarts which predate
		// the agent, which is why it is keyed per cluster and superseding
		// rather than windowed: an untimed total has no window to belong to
		// (ADR 0034).
		Kind:       "restart_counters",
		Source:     SourceJournal,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0034",
	},
	{
		Kind:       "pod_disruptions",
		Source:     SourceJournal,
		NaturalKey: "(window start, window length)",
		Delivery:   DeliverySupersedes,
		ADR:        "0021",
	},
	{
		// Windowed like the other two journals, and for the same reason: the
		// window bounds the spool's file count, which a fleet of CronJobs would
		// otherwise control (ADR 0029).
		Kind:       "job_runs",
		Source:     SourceJournal,
		NaturalKey: "(window start, window length)",
		Delivery:   DeliverySupersedes,
		ADR:        "0029",
	},
	{
		Kind:       "go_inventory",
		Source:     SourceStructural,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0010, 0017, 0018",
	},
	{
		// A reading, not a window: VmHWM is a high-water mark the kernel keeps
		// since a process started, so it belongs to no window — the shape
		// ADR 0034 settled for the restart counter. Measured, so it cannot ride
		// in `go_inventory` beside the structural facts read from the same
		// binary (ADR 0012 §2).
		Kind:       "process_peaks",
		Source:     SourceMeasured,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0052",
	},
	{
		// Structural, not measured: a bound port is a fact about how the
		// workload is configured, and reading it from the process rather than
		// from the spec does not make it an instrument reading (ADR 0012 §2).
		Kind:       "listening_ports",
		Source:     SourceStructural,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0056",
	},
	{
		Kind:       "go_build",
		Source:     SourceStructural,
		NaturalKey: "image digest",
		Delivery:   DeliveryWriteOnce,
		ADR:        "0017, 0019",
	},
	{
		Kind:       "workload_metadata",
		Source:     SourceStructural,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0012, 0014, 0021, 0031",
	},
	{
		// Current state, not history: Kubernetes keeps only
		// `revisionHistoryLimit` revisions, and accumulating past that is the
		// backend's job across snapshots (ADR 0018's reasoning, ADR 0030).
		Kind:       "workload_revisions",
		Source:     SourceStructural,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0030, 0049",
	},
	{
		// What bounds a workload from outside its own spec: budgets,
		// autoscalers, claims. A workload nothing constrains has no record
		// (ADR 0032).
		Kind:       "workload_policy",
		Source:     SourceStructural,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0032, 0033",
	},
	{
		// Namespace policy for collected namespaces, plus the two
		// cluster-scoped catalogs workloads point into by name (ADR 0032).
		Kind:       "cluster_policy",
		Source:     SourceStructural,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0032, 0033",
	},
	{
		Kind:       "node_metadata",
		Source:     SourceStructural,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0012, 0019",
	},
	{
		// The whole key is in the spool filename, digest included: two replicas
		// of one workload on one node cut the same window, so without the digest
		// the second capture replaced the first (ADR 0023).
		Kind:       "ebpf_profile",
		Source:     SourceSampled,
		NaturalKey: "(namespace, workload, container, image digest, capture start–end)",
		Delivery:   DeliveryAccumulates,
		ADR:        "0011, 0023",
	},
	{
		// The same class and the same key as the eBPF kind, and a separate kind
		// because the capture differs: the workload's own runtime sampled
		// itself, at its own rate, over a window the agent asked for. Which
		// capture produced a profile is what the kind names (ADR 0012 §2).
		Kind:       "pprof_profile",
		Source:     SourceSampled,
		NaturalKey: "(namespace, workload, container, image digest, capture start–end)",
		Delivery:   DeliveryAccumulates,
		ADR:        "0058",
	},
}

// Registry returns the payload kinds this agent ships, sorted by kind so the
// order is stable for anything that renders it.
func Registry() []PayloadKind {
	out := make([]PayloadKind, len(registry))
	copy(out, registry)
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Lookup returns the registry row for a payload kind.
func Lookup(kind string) (PayloadKind, bool) {
	for _, p := range registry {
		if p.Kind == kind {
			return p, true
		}
	}
	return PayloadKind{}, false
}
