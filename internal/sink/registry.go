package sink

import "sort"

// The payload registry: the one place that says what this agent ships.
//
// ADR 0012 §1 introduced this list as a table in a document, and the table did
// not survive contact with six later decisions — ADR 0013 completed the
// provenance column, ADR 0017 added a kind, ADR 0019 renamed it, ADR 0020 and
// ADR 0021 added two more. Reading ADR 0012 today gives a list that is wrong in
// three rows and missing three kinds, and nothing anywhere answers "what does
// the agent send". That is what ADR 0022 moves here.
//
// The table lives next to the writers and is checked against the golden payload
// bytes in both directions (registry_test.go): a kind that ships without a row
// fails, and a row with no shipped payload fails. A document cannot fail, which
// is the whole reason this is code.
//
// Adding a kind means adding a row here, a writer, and a golden — and an ADR
// recording the decision, exactly as ADR 0012 required. What changed is only
// where the list itself lives.

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
		Kind:       "deployment_revisions",
		Source:     SourceStructural,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0030",
	},
	{
		// What bounds a workload from outside its own spec: budgets,
		// autoscalers, claims. A workload nothing constrains has no record
		// (ADR 0032).
		Kind:       "workload_policy",
		Source:     SourceStructural,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0032",
	},
	{
		// Namespace policy for collected namespaces, plus the two
		// cluster-scoped catalogs workloads point into by name (ADR 0032).
		Kind:       "cluster_policy",
		Source:     SourceStructural,
		NaturalKey: "the kind itself (one per cluster)",
		Delivery:   DeliverySupersedes,
		ADR:        "0032",
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
