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

// Rollout is how a workload replaces its own replicas: the one thing that
// decides whether a routine deploy is also an outage. A budget says what an
// eviction may take away; this says what the workload takes away from itself,
// and no budget is consulted on that path (ADR 0048 §2).
//
// It is a fact of the workload, not of a pod or a build, so it is nested rather
// than flattened onto records keyed more narrowly (ADR 0014).
type Rollout struct {
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
	// rollout treats it as available. Zero is the default and is what makes a
	// crash-on-first-request roll through every replica.
	MinReadySeconds int32 `json:"min_ready_seconds,omitempty"`
	// Unread marks a workload whose controller is a kind the agent does not
	// read — an Argo Rollout, a Knative Revision, an in-house operator. It is
	// the only field set when it is true: the workload may well roll, and this
	// says the agent did not look, which is not the same fact as a kind that
	// does not roll at all (ADR 0048 §2, ADR 0013).
	Unread bool `json:"unread,omitempty"`
}

// rolloutlessKinds are the kinds known to replace no replicas: nothing about
// them is unobserved, so their absence from Rollouts is the fact itself.
var rolloutlessKinds = map[string]bool{
	"Job": true, "CronJob": true, "ReplicaSet": true, "none": true, "": true,
}

// Rollouts returns the update strategy of every workload with admitted pods.
// Scope is inherited from the admitted pod index rather than decided again, as
// with revisions (ADR 0030) and policy (ADR 0032). A kind with no strategy — a
// bare Job, a CronJob, an owner-less pod — is absent: a shape, not a gap.
//
// It is read at flush time and not when a pod was described, because a strategy
// can be edited with no pod event following it.
func (w *PodWatcher) Rollouts() map[WorkloadKey]Rollout {
	out := make(map[WorkloadKey]Rollout)
	for key := range w.collectedWorkloadKeys() {
		if rollout, ok := w.rolloutOf(key); ok {
			out[key] = rollout
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

// rolloutOf reads one workload's strategy from the cache its kind lives in. The
// three listers are gating caches (ADR 0035), so a miss here is informer lag or
// a workload that has just been deleted, never a permission the agent lacks.
func (w *PodWatcher) rolloutOf(key WorkloadKey) (Rollout, bool) {
	if rolloutlessKinds[key.Kind] {
		return Rollout{}, false
	}
	switch key.Kind {
	case "Deployment":
		if w.deployLister == nil {
			return Rollout{}, false
		}
		d, err := w.deployLister.Deployments(key.Namespace).Get(key.Name)
		if err != nil {
			return Rollout{}, false
		}
		r := Rollout{Type: string(d.Spec.Strategy.Type), MinReadySeconds: d.Spec.MinReadySeconds}
		if u := d.Spec.Strategy.RollingUpdate; u != nil {
			r.MaxUnavailable = intOrString(u.MaxUnavailable)
			r.MaxSurge = intOrString(u.MaxSurge)
		}
		return r, true
	case "StatefulSet":
		if w.stsLister == nil {
			return Rollout{}, false
		}
		s, err := w.stsLister.StatefulSets(key.Namespace).Get(key.Name)
		if err != nil {
			return Rollout{}, false
		}
		r := Rollout{Type: string(s.Spec.UpdateStrategy.Type), MinReadySeconds: s.Spec.MinReadySeconds}
		if u := s.Spec.UpdateStrategy.RollingUpdate; u != nil {
			r.Partition = u.Partition
			r.MaxUnavailable = intOrString(u.MaxUnavailable)
		}
		return r, true
	case "DaemonSet":
		if w.dsLister == nil {
			return Rollout{}, false
		}
		d, err := w.dsLister.DaemonSets(key.Namespace).Get(key.Name)
		if err != nil {
			return Rollout{}, false
		}
		r := Rollout{Type: string(d.Spec.UpdateStrategy.Type), MinReadySeconds: d.Spec.MinReadySeconds}
		if u := d.Spec.UpdateStrategy.RollingUpdate; u != nil {
			r.MaxUnavailable = intOrString(u.MaxUnavailable)
			r.MaxSurge = intOrString(u.MaxSurge)
		}
		return r, true
	}
	// A custom resource. Reading it would need RBAC on arbitrary CRDs, which
	// this product does not ask for (docs/security.md §11), so what the agent
	// can honestly say is that it did not look.
	return Rollout{Unread: true}, true
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
