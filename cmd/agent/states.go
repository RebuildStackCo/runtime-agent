package main

import (
	"math"

	"github.com/RebuildStackCo/runtime-agent/internal/journal"
	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofpull"
)

// agentStates is the pass's own states in the shape the window accumulates,
// built from the values the coverage snapshot was just written from so the two
// cannot disagree (ADR 0088 §2).
//
// Every watched source contributes, failing or not: a source with no record is
// one nobody watched, and that is the distinction the window exists to keep.
func agentStates(sources []model.SourceHealth, shipping *model.Shipping) []journal.AgentState {
	out := make([]journal.AgentState, 0, len(sources)+1)
	for _, src := range sources {
		out = append(out, journal.AgentState{
			State:   journal.StateFailing,
			Subject: src.Name,
			Since:   src.FailingSince,
		})
	}
	// Absent when the agent has no backend: an agent told to ship nowhere has
	// not stopped shipping, and an hour of "not halted" would claim it had a
	// backend to be halted from.
	if shipping != nil {
		out = append(out, journal.AgentState{
			State:   journal.StateHalted,
			Subject: journal.SubjectShip,
			Since:   shipping.HaltedSince,
		})
	}
	return out
}

// agentCounters is what this pass read of the agent's own counters, keyed by
// the path each number has where it is reported cumulatively (ADR 0089 §2).
//
// Only monotonic counters appear, and ADR 0089 §2 names what each exclusion
// was excluded for.
func agentCounters(filter model.Coverage, obs model.Observation,
	pull *pprofpull.Coverage, intake *model.IntakeRejections,
) map[string]int64 {
	out := map[string]int64{
		"filter.pods_observed":                      filter.PodsObserved,
		"filter.excluded_namespace_filter":          filter.ExcludedNamespaceFilter,
		"filter.excluded_namespace_annotation":      filter.ExcludedNamespaceAnnotation,
		"filter.excluded_workload_annotation":       filter.ExcludedWorkloadAnnotation,
		"filter.excluded_pod_annotation":            filter.ExcludedPodAnnotation,
		"filter.excluded_profiling_annotation":      filter.ExcludedProfilingAnnotation,
		"filter.workload_unknown_kind":              filter.WorkloadUnknownKind,
		"filter.workload_not_cached":                filter.WorkloadNotCached,
		"filter.jobs_observed":                      filter.JobsObserved,
		"filter.jobs_excluded_namespace_filter":     filter.JobsExcludedNamespaceFilter,
		"filter.jobs_excluded_namespace_annotation": filter.JobsExcludedNamespaceAnnotation,
		"filter.jobs_excluded_workload_annotation":  filter.JobsExcludedWorkloadAnnotation,
		"filter.jobs_excluded_annotation":           filter.JobsExcludedAnnotation,
		"observation.polls_attempted":               obs.PollsAttempted,
		"observation.polls_failed":                  obs.PollsFailed,
	}
	// Present only when the component that counts them is deployed. A counter
	// absent from the map writes no record, which is the same claim its block
	// makes by being absent from the coverage payload.
	if pull != nil {
		out["pprof_pull.shipped"] = int64(pull.Shipped)
		out["pprof_pull.refused"] = int64(pull.Refused)
		out["pprof_pull.unreachable"] = int64(pull.Unreachable)
		out["pprof_pull.invalid"] = int64(pull.Invalid)
	}
	if intake != nil {
		out["intake_rejections.unauthorized"] = asCount(intake.Unauthorized)
		out["intake_rejections.too_large"] = asCount(intake.TooLarge)
		out["intake_rejections.malformed"] = asCount(intake.Malformed)
	}
	return out
}

// asCount narrows a counter the model keeps unsigned. Saturating rather than
// wrapping: a wrapped value would read as a counter that fell and be dropped
// in silence, where a pinned one stays visibly wrong (ADR 0089 §4).
func asCount(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}
