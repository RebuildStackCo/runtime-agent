package collector

import (
	"strings"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
)

// CollectAnnotation set to "false" on a namespace or pod opts it out of
// collection without touching the agent's configuration.
const CollectAnnotation = "rebuildstack.co/collect"

// ExclusionReason names the filter that rejected a pod. Reasons are the only
// thing ever reported about excluded pods — their identities stay in the
// cluster; only aggregate counts per reason leave it.
type ExclusionReason string

// Exclusion reasons, in evaluation order: the configured namespace
// allow/deny lists, the namespace's opt-out annotation, the pod's own.
const (
	ExcludedByNamespaceFilter     ExclusionReason = "namespace_filter"
	ExcludedByNamespaceAnnotation ExclusionReason = "namespace_annotation"
	ExcludedByPodAnnotation       ExclusionReason = "pod_annotation"
)

// PodFilter decides which pods are collected and counts what it excludes.
// The zero rules (no allow, no deny) admit every namespace; the opt-out
// annotations are always honored regardless of configuration.
type PodFilter struct {
	allow []string
	deny  []string

	podsObserved                atomic.Int64
	excludedNamespaceFilter     atomic.Int64
	excludedNamespaceAnnotation atomic.Int64
	excludedPodAnnotation       atomic.Int64
}

// NewPodFilter builds a filter from namespace name lists; entries may use
// "*" as a wildcard. An empty allow list admits every namespace, a non-empty
// one admits only matches. Deny always applies on top and wins on conflict.
func NewPodFilter(allowNamespaces, denyNamespaces []string) *PodFilter {
	return &PodFilter{allow: allowNamespaces, deny: denyNamespaces}
}

// Admit reports whether the pod passes the filters and, when it does not,
// the first filter that rejected it: namespace allow/deny, then the
// namespace's opt-out annotation, then the pod's own.
func (f *PodFilter) Admit(pod *corev1.Pod, nsAnnotations map[string]string) (bool, ExclusionReason) {
	if !f.namespaceAllowed(pod.Namespace) {
		return false, ExcludedByNamespaceFilter
	}
	if nsAnnotations[CollectAnnotation] == "false" {
		return false, ExcludedByNamespaceAnnotation
	}
	if pod.Annotations[CollectAnnotation] == "false" {
		return false, ExcludedByPodAnnotation
	}
	return true, ""
}

func (f *PodFilter) namespaceAllowed(namespace string) bool {
	if len(f.allow) > 0 && !matchAny(f.allow, namespace) {
		return false
	}
	return !matchAny(f.deny, namespace)
}

func (f *PodFilter) countObserved() {
	f.podsObserved.Add(1)
}

func (f *PodFilter) countExcluded(reason ExclusionReason) {
	switch reason {
	case ExcludedByNamespaceFilter:
		f.excludedNamespaceFilter.Add(1)
	case ExcludedByNamespaceAnnotation:
		f.excludedNamespaceAnnotation.Add(1)
	case ExcludedByPodAnnotation:
		f.excludedPodAnnotation.Add(1)
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
	ExcludedPodAnnotation       int64
}

// Snapshot returns the current coverage counters.
func (f *PodFilter) Snapshot() Coverage {
	return Coverage{
		PodsObserved:                f.podsObserved.Load(),
		ExcludedNamespaceFilter:     f.excludedNamespaceFilter.Load(),
		ExcludedNamespaceAnnotation: f.excludedNamespaceAnnotation.Load(),
		ExcludedPodAnnotation:       f.excludedPodAnnotation.Load(),
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
