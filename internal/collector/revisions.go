package collector

import (
	"strconv"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// revisionAnnotation is where the Deployment controller records which revision a
// ReplicaSet is. It is Kubernetes' own annotation, not one this agent writes,
// and it is the only revision key the agent knows (ADR 0049 §3).
const revisionAnnotation = "deployment.kubernetes.io/revision"

// ReplicaSets returns the collected view of every ReplicaSet whose controller is
// a workload with admitted pods — a Deployment, or any custom resource that
// manages ReplicaSets, which is how Argo Rollouts and its kind work (ADR 0049).
//
// Which workloads those are is inherited, not decided again: the set is read
// from the admitted pod index, so an excluded workload is absent here too. A
// workload whose pods are all gone therefore has no revisions even while its
// ReplicaSets exist — the forgetting ADR 0018 chose for the Go inventory.
func (w *PodWatcher) ReplicaSets() []model.ReplicaSetInfo {
	owners := w.replicaSetOwners()
	if len(owners) == 0 {
		return nil
	}
	sets, err := w.rsLister.List(labels.Everything())
	if err != nil {
		return nil
	}

	out := make([]model.ReplicaSetInfo, 0, len(sets))
	for _, set := range sets {
		// A ReplicaSet with no controller is a bare one, managed directly. It
		// is the workload rather than a revision of one, so it has no history
		// to report here (ADR 0049 §2).
		owner := metav1.GetControllerOf(set)
		if owner == nil {
			continue
		}
		key := model.WorkloadKey{Namespace: set.Namespace, Kind: owner.Kind, Name: owner.Name}
		if !owners[key] {
			continue
		}
		out = append(out, describeReplicaSet(set, key))
	}
	return out
}

type workloadKey struct {
	namespace string
	name      string
}

// replicaSetOwners is every workload that currently has at least one admitted
// pod, keyed by kind as well as name. Kinds that cannot own a ReplicaSet are not
// filtered out: no set will ever name them, so they cost a map entry and no
// correctness.
func (w *PodWatcher) replicaSetOwners() map[model.WorkloadKey]bool {
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()
	out := make(map[model.WorkloadKey]bool)
	for _, entry := range w.index {
		out[model.WorkloadKey{
			Namespace: entry.namespace,
			Kind:      entry.workload.Kind,
			Name:      entry.workload.Name,
		}] = true
	}
	return out
}

func describeReplicaSet(set *appsv1.ReplicaSet, owner model.WorkloadKey) model.ReplicaSetInfo {
	info := model.ReplicaSetInfo{
		Namespace:       set.Namespace,
		Workload:        model.WorkloadRef{Kind: owner.Kind, Name: owner.Name},
		Name:            set.Name,
		Revision:        revisionOf(set, owner.Kind),
		CreatedAt:       set.CreationTimestamp.UTC(),
		CurrentReplicas: set.Status.Replicas,
		ReadyReplicas:   set.Status.ReadyReplicas,
	}
	if set.Spec.Replicas != nil {
		info.DesiredReplicas = *set.Spec.Replicas
	}
	for _, c := range set.Spec.Template.Spec.InitContainers {
		info.Containers = append(info.Containers,
			model.RevisionContainer{Name: c.Name, Image: c.Image, Init: true})
	}
	for _, c := range set.Spec.Template.Spec.Containers {
		info.Containers = append(info.Containers,
			model.RevisionContainer{Name: c.Name, Image: c.Image})
	}
	return info
}

// revisionOf parses the Deployment controller's revision annotation, and only
// for a Deployment. Every other controller numbers revisions under a key of its
// own, and no finding needs the number — order comes from `created_at`, so
// reading a vendor's key would put its vocabulary in this agent for nothing
// (ADR 0049 §3). An absent or malformed value yields nil.
func revisionOf(set *appsv1.ReplicaSet, kind string) *int64 {
	if kind != "Deployment" {
		return nil
	}
	raw, ok := set.Annotations[revisionAnnotation]
	if !ok {
		return nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}
