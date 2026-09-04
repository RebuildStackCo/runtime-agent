package model

// Coverage is an aggregate snapshot of what the filter admitted and
// excluded, counted once per pod appearance. It is the seed of the coverage
// report (docs/security.md §11): full information about what is collected,
// only counts about what is not.
type Coverage struct {
	PodsObserved                int64 `json:"pods_observed"`
	ExcludedNamespaceFilter     int64 `json:"excluded_namespace_filter"`
	ExcludedNamespaceAnnotation int64 `json:"excluded_namespace_annotation"`
	ExcludedWorkloadAnnotation  int64 `json:"excluded_workload_annotation"`
	ExcludedPodAnnotation       int64 `json:"excluded_pod_annotation"`
	// WorkloadUnknownKind and WorkloadNotCached are the blind spot: pods
	// admitted without their workload-level opt-out being checked. They are
	// kept apart because they mean different things — the first is a standing
	// property of a cluster running operators the agent does not read, the
	// second should sit at zero.
	WorkloadUnknownKind int64 `json:"workload_unknown_kind"`
	WorkloadNotCached   int64 `json:"workload_not_cached"`

	// Job counters, kept apart from the pod ones on purpose: one Job produces
	// many pods, so a shared counter would answer neither question.
	JobsObserved                    int64 `json:"jobs_observed"`
	JobsExcludedNamespaceFilter     int64 `json:"jobs_excluded_namespace_filter"`
	JobsExcludedNamespaceAnnotation int64 `json:"jobs_excluded_namespace_annotation"`
	JobsExcludedWorkloadAnnotation  int64 `json:"jobs_excluded_workload_annotation"`
	JobsExcludedAnnotation          int64 `json:"jobs_excluded_annotation"`
}

// PlacementDrops is what the placement reduction refused to carry, for the
// coverage report.
type PlacementDrops struct {
	Values int64 `json:"values_dropped"`
	Terms  int64 `json:"terms_dropped"`
}

// SourceHealth is one watched source as the agent actually found it: the
// agent's effective read access, measured rather than declared. A grant the
// ClusterRole holds but a webhook defeats reads as granted in any review of the
// rules, and as failing here (ADR 0054 §3).
type SourceHealth struct {
	// Name is the resource class, not a customer object: "services",
	// "endpoint_slices", and so on.
	Name string `json:"name"`
	// Synced is whether the cache ever filled.
	Synced bool `json:"synced"`
	// Failing is whether its watch has been erroring recently enough to treat
	// the cache as no longer fed (ADR 0035).
	Failing bool `json:"failing,omitempty"`
}

// NodeDrops is what the node reductions refused to carry, cumulative since the
// process started. It reaches the coverage report so a fleet whose nodes do not
// fit these bounds is visible rather than quietly under-described.
type NodeDrops struct {
	Conditions int64 `json:"conditions_dropped"`
	Devices    int64 `json:"devices_dropped"`
	Taints     int64 `json:"taints_dropped"`
	Values     int64 `json:"values_dropped"`
}

// Observation is what the agent knows about its own collection: the polling
// cadence, how many kubelet requests it made and how many failed, and which
// signals this cluster exposes. It ships with the usage payloads (ADR 0012)
// because only the agent can know a scrape failed — from outside, a failed
// scrape and a quiet container are indistinguishable.
//
// Counters are cumulative since agent start and cluster-wide; per-record
// coverage lives in the record's own sample counts and CoveredNanoseconds.
type Observation struct {
	PollIntervalSeconds int64    `json:"poll_interval_seconds"`
	PollsAttempted      int64    `json:"polls_attempted"`
	PollsFailed         int64    `json:"polls_failed"`
	Signals             []string `json:"signals"`
}

// IntakeRejections is what the node-intake receiver refused on its two push
// paths, by reason, cumulative since the controller started. It says why a node
// has gone quiet where asserted_at only says that it has; a refused report
// appears nowhere else but that node's own log (ADR 0067).
//
// Aggregate only: no identity of a refused caller appears, and no number here
// names a node (CLAUDE.md invariant 6).
type IntakeRejections struct {
	// Unauthorized is a token that did not verify — a misconfigured audience or
	// subject, an expired projection, or a caller that is not the node role.
	Unauthorized uint64 `json:"unauthorized"`
	// TooLarge is a body over the receiver's bound. The node that sent it holds
	// more than the channel can carry, and re-sending will not help.
	TooLarge uint64 `json:"too_large"`
	// Malformed is a body the receiver could not decode, including one carrying
	// a field this build's schema does not have.
	Malformed uint64 `json:"malformed"`
}

// Plus adds two rejection counts. The receiver's two push paths keep their own
// counters; the coverage report states the channel, so they are summed here.
func (r IntakeRejections) Plus(o IntakeRejections) IntakeRejections {
	return IntakeRejections{
		Unauthorized: r.Unauthorized + o.Unauthorized,
		TooLarge:     r.TooLarge + o.TooLarge,
		Malformed:    r.Malformed + o.Malformed,
	}
}
