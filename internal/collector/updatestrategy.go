package collector

import (
	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// UpdateStrategies returns the declared strategy of every workload with admitted
// pods whose kind the agent reads: Deployment, StatefulSet and DaemonSet. Any
// other kind is absent, and absence claims nothing about whether that workload
// updates itself — only that this agent did not read how (ADR 0048 §2).
//
// Scope is inherited from the admitted pod index (ADR 0030, ADR 0032). It is
// read at flush time and not when a pod was described, because a strategy can
// be edited with no pod event following it.
func (w *PodWatcher) UpdateStrategies() map[model.WorkloadKey]model.UpdateStrategy {
	out := make(map[model.WorkloadKey]model.UpdateStrategy)
	for key := range w.collectedWorkloadKeys() {
		if strategy, ok := w.strategyOf(key); ok {
			out[key] = strategy
		}
	}
	return out
}

// collectedWorkloadKeys is every workload with at least one admitted pod.
func (w *PodWatcher) collectedWorkloadKeys() map[model.WorkloadKey]struct{} {
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()
	out := make(map[model.WorkloadKey]struct{})
	for _, entry := range w.index {
		out[model.WorkloadKey{
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
func (w *PodWatcher) strategyOf(key model.WorkloadKey) (model.UpdateStrategy, bool) {
	switch key.Kind {
	case "Deployment":
		if w.deployLister == nil {
			return model.UpdateStrategy{}, false
		}
		d, err := w.deployLister.Deployments(key.Namespace).Get(key.Name)
		if err != nil {
			return model.UpdateStrategy{}, false
		}
		s := model.UpdateStrategy{Type: string(d.Spec.Strategy.Type), MinReadySeconds: d.Spec.MinReadySeconds}
		if u := d.Spec.Strategy.RollingUpdate; u != nil {
			s.MaxUnavailable = intOrString(u.MaxUnavailable)
			s.MaxSurge = intOrString(u.MaxSurge)
		}
		return s, true
	case "StatefulSet":
		if w.stsLister == nil {
			return model.UpdateStrategy{}, false
		}
		sts, err := w.stsLister.StatefulSets(key.Namespace).Get(key.Name)
		if err != nil {
			return model.UpdateStrategy{}, false
		}
		s := model.UpdateStrategy{Type: string(sts.Spec.UpdateStrategy.Type), MinReadySeconds: sts.Spec.MinReadySeconds}
		if u := sts.Spec.UpdateStrategy.RollingUpdate; u != nil {
			s.Partition = u.Partition
			s.MaxUnavailable = intOrString(u.MaxUnavailable)
		}
		return s, true
	case "DaemonSet":
		if w.dsLister == nil {
			return model.UpdateStrategy{}, false
		}
		d, err := w.dsLister.DaemonSets(key.Namespace).Get(key.Name)
		if err != nil {
			return model.UpdateStrategy{}, false
		}
		s := model.UpdateStrategy{Type: string(d.Spec.UpdateStrategy.Type), MinReadySeconds: d.Spec.MinReadySeconds}
		if u := d.Spec.UpdateStrategy.RollingUpdate; u != nil {
			s.MaxUnavailable = intOrString(u.MaxUnavailable)
			s.MaxSurge = intOrString(u.MaxSurge)
		}
		return s, true
	}
	return model.UpdateStrategy{}, false
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
