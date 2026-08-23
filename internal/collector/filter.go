package collector

import (
	"strings"
	"sync/atomic"

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
// The workload step is deliberately fail-open: a pod whose controller could
// not be read is admitted, and the reason it could not be read is counted.
// This product is opt-out — an empty allow list collects everything — so an
// unreadable controller is not evidence that anyone opted out; an annotation
// is a rare deliberate act. Failing closed here would silently collect
// nothing on every cluster running an operator the agent does not know, which
// is most of the sophisticated ones (ADR 0028). The customer keeps two
// working controls in that case, the namespace and the pod.
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
	PodsObserved                int64
	ExcludedNamespaceFilter     int64
	ExcludedNamespaceAnnotation int64
	ExcludedWorkloadAnnotation  int64
	ExcludedPodAnnotation       int64
	// WorkloadUnknownKind and WorkloadNotCached are the blind spot: pods
	// admitted without their workload-level opt-out being checked. They are
	// kept apart because they mean different things — the first is a standing
	// property of a cluster running operators the agent does not read, the
	// second should sit at zero.
	WorkloadUnknownKind int64
	WorkloadNotCached   int64
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
