package nodescan

// Report is the node role's scan result as delivered to the controller
// (ADR 0010): the kept (non-infrastructure) binaries and the aggregate counters
// for one pass, tagged with the node that produced them. It is the wire
// contract of the node→controller channel — the node marshals it, the
// controller unmarshals it — and carries only already-filtered facts (the
// on-node module filter, ADR 0009, ran before this shape exists). No identity
// of a filtered-out or unreadable binary appears here; those are only the
// counters (CLAUDE.md invariant 6).
type Report struct {
	Node     string       `json:"node"`
	Binaries []BinaryInfo `json:"binaries"`
	Counters Counters     `json:"counters"`
	// Profiling is what the profiler beside the scanner did, absent when this
	// build of the node does not report it (ADR 0060).
	Profiling *ProfilingCoverage `json:"profiling,omitempty"`
}

// ProfilingCoverage is what the node's eBPF profiler did, as a node reports it.
// It rides the scanner's report because that is the only channel that fires
// when the profiler does not: a node whose kernel refused the gate ships no
// profile, and it is that node whose silence has to be explained (ADR 0060).
//
// Counts are cumulative since the node started — unlike Counters above, which
// are one pass — and a restart starts them again, which is the only thing a
// node holding no state can say (ADR 0060 §3).
type ProfilingCoverage struct {
	// State is why this node is or is not profiling, one of: "supported",
	// "disabled" (the master switch is off), "program_load_failed",
	// "capture_stopped", or a gate refusal — "kernel_too_old", "btf_absent",
	// "kernel_unknown". One node has one state (ADR 0060 §2, §5).
	State string `json:"state"`
	// Windows is every capture window this node cut. The three below are the
	// windows that produced nothing and why: no scope from the controller (the
	// fail-closed path), no targets to profile, no samples captured at all.
	// A profiler that loads and never sees a sample is a broken agent and looks
	// like an idle cluster without this count.
	Windows          int `json:"windows"`
	WindowsNoScope   int `json:"windows_no_scope"`
	WindowsNoTargets int `json:"windows_no_targets"`
	WindowsNoSamples int `json:"windows_no_samples"`
	// ProfilesShipped is what reached the controller. Invalid is what
	// nodeprofile.Validate refused — a profile of nothing but runtime and
	// redacted frames — and Unshipped is a delivery that failed.
	ProfilesShipped   int `json:"profiles_shipped"`
	ProfilesInvalid   int `json:"profiles_invalid"`
	ProfilesUnshipped int `json:"profiles_unshipped"`
	// SamplesOutOfScope is targeted samples dropped because their pod is not in
	// the controller's own scan scope: informer lag, or a controller
	// disagreeing with itself.
	SamplesOutOfScope int `json:"samples_out_of_scope"`
	// What the symbol filter redacted, summed over every profile this node
	// built. A rising third-party count on a cluster whose services are the
	// customer's own is the signature of an allow-list that stopped matching
	// (ADR 0059), which is otherwise invisible until someone reads a profile.
	ThirdPartyDropped   uint64 `json:"third_party_dropped"`
	UnsymbolizedDropped uint64 `json:"unsymbolized_dropped"`
	SamplesFiltered     uint64 `json:"samples_filtered"`
}
