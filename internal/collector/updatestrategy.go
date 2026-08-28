package collector

import (
	"k8s.io/apimachinery/pkg/util/intstr"
)

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

// UpdateStrategies returns the declared strategy of every workload with admitted
// pods whose kind the agent reads: Deployment, StatefulSet and DaemonSet. Any
// other kind is absent, and absence claims nothing about whether that workload
// updates itself — only that this agent did not read how (ADR 0048 §2).
//
// Scope is inherited from the admitted pod index (ADR 0030, ADR 0032). It is
// read at flush time and not when a pod was described, because a strategy can
// be edited with no pod event following it.
func (w *PodWatcher) UpdateStrategies() map[WorkloadKey]UpdateStrategy {
	out := make(map[WorkloadKey]UpdateStrategy)
	for key := range w.collectedWorkloadKeys() {
		if strategy, ok := w.strategyOf(key); ok {
			out[key] = strategy
		}
	}
	return out
}

// collectedWorkloadKeys is every workload with at least one admitted pod.
func (w *PodWatcher) collectedWorkloadKeys() map[WorkloadKey]struct{} {
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()
	out := make(map[WorkloadKey]struct{})
	for _, entry := range w.index {
		out[WorkloadKey{
			Namespace: entry.namespace,
			Kind:      entry.workload.Kind,
			Name:      entry.workload.Name,
		}] = struct{}{}
	}
	return out
}

// strategyOf reads one workload's strategy from the cache its kind lives in. The
// three listers are gating caches (ADR 0035), so a miss here is informer lag or
// a workload that has just been deleted, never a permission the agent lacks.
func (w *PodWatcher) strategyOf(key WorkloadKey) (UpdateStrategy, bool) {
	switch key.Kind {
	case "Deployment":
		if w.deployLister == nil {
			return UpdateStrategy{}, false
		}
		d, err := w.deployLister.Deployments(key.Namespace).Get(key.Name)
		if err != nil {
			return UpdateStrategy{}, false
		}
		s := UpdateStrategy{Type: string(d.Spec.Strategy.Type), MinReadySeconds: d.Spec.MinReadySeconds}
		if u := d.Spec.Strategy.RollingUpdate; u != nil {
			s.MaxUnavailable = intOrString(u.MaxUnavailable)
			s.MaxSurge = intOrString(u.MaxSurge)
		}
		return s, true
	case "StatefulSet":
		if w.stsLister == nil {
			return UpdateStrategy{}, false
		}
		sts, err := w.stsLister.StatefulSets(key.Namespace).Get(key.Name)
		if err != nil {
			return UpdateStrategy{}, false
		}
		s := UpdateStrategy{Type: string(sts.Spec.UpdateStrategy.Type), MinReadySeconds: sts.Spec.MinReadySeconds}
		if u := sts.Spec.UpdateStrategy.RollingUpdate; u != nil {
			s.Partition = u.Partition
			s.MaxUnavailable = intOrString(u.MaxUnavailable)
		}
		return s, true
	case "DaemonSet":
		if w.dsLister == nil {
			return UpdateStrategy{}, false
		}
		d, err := w.dsLister.DaemonSets(key.Namespace).Get(key.Name)
		if err != nil {
			return UpdateStrategy{}, false
		}
		s := UpdateStrategy{Type: string(d.Spec.UpdateStrategy.Type), MinReadySeconds: d.Spec.MinReadySeconds}
		if u := d.Spec.UpdateStrategy.RollingUpdate; u != nil {
			s.MaxUnavailable = intOrString(u.MaxUnavailable)
			s.MaxSurge = intOrString(u.MaxSurge)
		}
		return s, true
	}
	return UpdateStrategy{}, false
}

// intOrString renders a declaration that may be a count or a percentage as it
// was written. A nil pointer is the field being unset, which the API server
// defaults — so it reads as absent rather than as zero.
func intOrString(v *intstr.IntOrString) string {
	if v == nil {
		return ""
	}
	return v.String()
}
