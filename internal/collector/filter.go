package collector

import (
	"strings"
	"sync/atomic"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// CollectAnnotation set to "false" on a namespace, a workload, or a pod opts
// it out of collection without touching the agent's configuration.
//
// On a workload it means the controller object itself — the Deployment, the
// StatefulSet, the DaemonSet, the CronJob, or a bare Job or ReplicaSet — and
// deliberately not that controller's pod template. An annotation in a pod
// template is part of the template hash, so writing one there rolls every
// replica; opting out of telemetry must not restart production (ADR 0028).
const CollectAnnotation = "rebuildstack.co/collect"

// ExclusionReason names the filter that rejected a pod. Reasons are the only
// thing ever reported about excluded pods — their identities stay in the
// cluster; only aggregate counts per reason leave it.
type ExclusionReason string

// Exclusion reasons, in evaluation order: the configured namespace
// allow/deny lists, the namespace's opt-out annotation, the workload's, the
// pod's own. The workload is checked before the pod because a workload that
// opted out usually leaves no annotation on its pods at all, and the reported
// reason should name the object the customer actually wrote on.
const (
	ExcludedByNamespaceFilter     ExclusionReason = "namespace_filter"
	ExcludedByNamespaceAnnotation ExclusionReason = "namespace_annotation"
	ExcludedByWorkloadAnnotation  ExclusionReason = "workload_annotation"
	ExcludedByPodAnnotation       ExclusionReason = "pod_annotation"
	// ExcludedByObjectAnnotation is the object's own annotation where that
	// object is not a pod — today a Job, whose own metadata is where an
	// annotation on a CronJob's `jobTemplate.metadata` lands, and whose pod
	// template is where an opt-out written the pre-ADR-0028 way lives. The two
	// are one reason because they mean one thing: this run was opted out.
	ExcludedByObjectAnnotation ExclusionReason = "object_annotation"
)

// UnresolvedWorkload says why a pod's workload annotations could not be read.
// Empty means they were read — including the case of a pod with no controller
// at all, which has no workload to consult and is not a failure.
type UnresolvedWorkload string

const (
	// WorkloadKindUnknown is a pod owned by a kind this agent does not read:
	// an Argo Rollout, a Knative Revision, any operator's CRD. Reading them
	// would need RBAC on arbitrary custom resources, which the product does
	// not ask for (security.md §4).
	WorkloadKindUnknown UnresolvedWorkload = "unknown_kind"
	// WorkloadNotCached is a controller of a known kind that was not in the
	// informer cache: a resync race, or an object deleted between the pod
	// event and the lookup. Transient, and a persistently non-zero count is a
	// defect in the agent rather than a property of the cluster.
	WorkloadNotCached UnresolvedWorkload = "not_cached"
)

// WorkloadLookup is what the caller could learn about the controller that
// manages a pod. Annotations is nil when Unresolved is set, and also when the
// pod has no controller.
type WorkloadLookup struct {
	Annotations map[string]string
	Unresolved  UnresolvedWorkload
}

// Filter decides which objects are collected and counts what it excludes.
// The zero rules (no allow, no deny) admit every namespace; the opt-out
// annotations are always honored regardless of configuration.
type Filter struct {
	allow []string
	deny  []string

	podsObserved                atomic.Int64
	excludedNamespaceFilter     atomic.Int64
	excludedNamespaceAnnotation atomic.Int64
	excludedWorkloadAnnotation  atomic.Int64
	excludedPodAnnotation       atomic.Int64

	workloadUnknownKind atomic.Int64
	workloadNotCached   atomic.Int64

	// Jobs are counted apart from pods. Folding them together would make
	// `pods_observed` state a falsehood about pods, and the coverage report is
	// the one place the customer is told what was not collected.
	jobsObserved                    atomic.Int64
	jobsExcludedNamespaceFilter     atomic.Int64
	jobsExcludedNamespaceAnnotation atomic.Int64
	jobsExcludedWorkloadAnnotation  atomic.Int64
	jobsExcludedAnnotation          atomic.Int64
}

// NewFilter builds a filter from namespace name lists; entries may use
// "*" as a wildcard. An empty allow list admits every namespace, a non-empty
// one admits only matches. Deny always applies on top and wins on conflict.
func NewFilter(allowNamespaces, denyNamespaces []string) *Filter {
	return &Filter{allow: allowNamespaces, deny: denyNamespaces}
}

// AdmitPod reports whether the pod passes the filters and, when it does not,
// the first filter that rejected it.
//
// The workload step is deliberately fail-open: an unreadable controller is not
// evidence that anyone opted out, and failing closed would silently collect
// nothing on every cluster running an operator the agent does not know
// (ADR 0028). The reason is counted, and the customer keeps two working
// controls, the namespace and the pod.
func (f *Filter) AdmitPod(pod *corev1.Pod, nsAnnotations map[string]string, workload WorkloadLookup) (bool, ExclusionReason) {
	if !f.namespaceAllowed(pod.Namespace) {
		return false, ExcludedByNamespaceFilter
	}
	if nsAnnotations[CollectAnnotation] == "false" {
		return false, ExcludedByNamespaceAnnotation
	}
	if workload.Annotations[CollectAnnotation] == "false" {
		return false, ExcludedByWorkloadAnnotation
	}
	if pod.Annotations[CollectAnnotation] == "false" {
		return false, ExcludedByPodAnnotation
	}
	return true, ""
}

// AdmitJob reports whether a finished Job's facts may be collected.
//
// The pod decision with one step added: the workload step is the owning CronJob,
// the object step the Job's own annotations, and the last step the Job's pod
// template, which is what excluded this run's pods if the customer opted out the
// way security.md documented before ADR 0028. Without it the agent ships
// `job_runs` for a workload whose pods it refuses to measure. Template
// annotations are metadata, never the `env` beneath them (invariant 4).
func (f *Filter) AdmitJob(job *batchv1.Job, nsAnnotations map[string]string, workload WorkloadLookup) (bool, ExclusionReason) {
	if !f.namespaceAllowed(job.Namespace) {
		return false, ExcludedByNamespaceFilter
	}
	if nsAnnotations[CollectAnnotation] == "false" {
		return false, ExcludedByNamespaceAnnotation
	}
	if workload.Annotations[CollectAnnotation] == "false" {
		return false, ExcludedByWorkloadAnnotation
	}
	if job.Annotations[CollectAnnotation] == "false" ||
		job.Spec.Template.Annotations[CollectAnnotation] == "false" {
		return false, ExcludedByObjectAnnotation
	}
	return true, ""
}

func (f *Filter) namespaceAllowed(namespace string) bool {
	if len(f.allow) > 0 && !matchAny(f.allow, namespace) {
		return false
	}
	return !matchAny(f.deny, namespace)
}

func (f *Filter) countObserved() {
	f.podsObserved.Add(1)
}

func (f *Filter) countExcluded(reason ExclusionReason) {
	switch reason {
	case ExcludedByNamespaceFilter:
		f.excludedNamespaceFilter.Add(1)
	case ExcludedByNamespaceAnnotation:
		f.excludedNamespaceAnnotation.Add(1)
	case ExcludedByWorkloadAnnotation:
		f.excludedWorkloadAnnotation.Add(1)
	case ExcludedByPodAnnotation:
		f.excludedPodAnnotation.Add(1)
	}
}

func (f *Filter) countJobObserved() {
	f.jobsObserved.Add(1)
}

func (f *Filter) countJobExcluded(reason ExclusionReason) {
	switch reason {
	case ExcludedByNamespaceFilter:
		f.jobsExcludedNamespaceFilter.Add(1)
	case ExcludedByNamespaceAnnotation:
		f.jobsExcludedNamespaceAnnotation.Add(1)
	case ExcludedByWorkloadAnnotation:
		f.jobsExcludedWorkloadAnnotation.Add(1)
	case ExcludedByObjectAnnotation:
		f.jobsExcludedAnnotation.Add(1)
	}
}

// countUnresolvedWorkload records that one pod was admitted without its
// workload-level opt-out being checked. It is counted on every admission
// decision, not only on exclusions: the number is the size of the blind spot,
// and it is only meaningful against the pods that were collected.
func (f *Filter) countUnresolvedWorkload(reason UnresolvedWorkload) {
	switch reason {
	case WorkloadKindUnknown:
		f.workloadUnknownKind.Add(1)
	case WorkloadNotCached:
		f.workloadNotCached.Add(1)
	}
}

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

// Snapshot returns the current coverage counters.
func (f *Filter) Snapshot() Coverage {
	return Coverage{
		PodsObserved:                f.podsObserved.Load(),
		ExcludedNamespaceFilter:     f.excludedNamespaceFilter.Load(),
		ExcludedNamespaceAnnotation: f.excludedNamespaceAnnotation.Load(),
		ExcludedWorkloadAnnotation:  f.excludedWorkloadAnnotation.Load(),
		ExcludedPodAnnotation:       f.excludedPodAnnotation.Load(),
		WorkloadUnknownKind:         f.workloadUnknownKind.Load(),
		WorkloadNotCached:           f.workloadNotCached.Load(),

		JobsObserved:                    f.jobsObserved.Load(),
		JobsExcludedNamespaceFilter:     f.jobsExcludedNamespaceFilter.Load(),
		JobsExcludedNamespaceAnnotation: f.jobsExcludedNamespaceAnnotation.Load(),
		JobsExcludedWorkloadAnnotation:  f.jobsExcludedWorkloadAnnotation.Load(),
		JobsExcludedAnnotation:          f.jobsExcludedAnnotation.Load(),
	}
}

func matchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if matchGlob(p, s) {
			return true
		}
	}
	return false
}

// matchGlob matches s against a pattern where "*" stands for any run of
// characters (possibly empty). Only "*" is special — deliberately not full
// path.Match syntax, so namespace names with "?" or "[" need no escaping.
func matchGlob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		idx := strings.Index(s, part)
		if idx < 0 {
			return false
		}
		s = s[idx+len(part):]
	}
	return strings.HasSuffix(s, last) && len(s) >= len(last)
}
