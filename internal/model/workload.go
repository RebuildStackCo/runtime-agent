package model

// WorkloadRef identifies the controller that ultimately manages a pod:
// Deployment, StatefulSet, DaemonSet, CronJob, a bare Job, an Argo Rollout,
// or any other CRD that owns pods through the standard owner-reference chain.
// Kind is "none" for pods with no controller.
type WorkloadRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// WorkloadKey identifies a workload, for the facts that belong to the whole
// workload rather than to one of its pods or one of its builds. It carries the
// kind as well as the name because a Deployment and a StatefulSet may share a
// name in one namespace.
type WorkloadKey struct {
	Namespace string
	Kind      string
	Name      string
}

// UpdateStrategy is how a workload replaces its own replicas: the one thing that
// decides whether a routine deploy is also an outage. A budget says what an
// eviction may take away; this says what the workload takes away from itself,
// and no budget is consulted on that path (ADR 0048 §2).
//
// It is named for Kubernetes' own field rather than for the process, because
// `Rollout` is the kind name of a custom resource this agent reports in
// `workload_kind` — one word for both would collide in a single record.
type UpdateStrategy struct {
	// Type is the strategy in Kubernetes' own vocabulary: RollingUpdate or
	// Recreate for a Deployment, RollingUpdate or OnDelete for a StatefulSet or
	// DaemonSet.
	Type string `json:"type,omitempty"`
	// MaxUnavailable and MaxSurge are kept as written, because either may be a
	// count or a percentage and the two are not interchangeable when the
	// replica count changes — the same reason a budget keeps its own
	// declaration verbatim (ADR 0032).
	MaxUnavailable string `json:"max_unavailable,omitempty"`
	MaxSurge       string `json:"max_surge,omitempty"`
	// Partition is a StatefulSet's held-back ordinal. A partition above zero
	// means an update that was started and deliberately not finished, which
	// looks like a stalled rollout from every other angle.
	Partition *int32 `json:"partition,omitempty"`
	// MinReadySeconds is how long a new replica must stay ready before the
	// update treats it as available. Zero is the default and is what makes a
	// crash-on-first-request roll through every replica.
	MinReadySeconds int32 `json:"min_ready_seconds,omitempty"`
}
