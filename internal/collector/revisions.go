package collector

import (
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// revisionAnnotation is where the Deployment controller records which revision a
// ReplicaSet is. It is Kubernetes' own annotation, not one this agent writes.
const revisionAnnotation = "deployment.kubernetes.io/revision"

// RevisionContainer is one container of a revision's pod template: the name and
// the image reference, and nothing else from the template.
//
// The image is what makes a revision mean something — "revision 47 is this
// build" is the fact that turns a usage change into an attributable one. The
// `env`, `args` and `command` beside it in the same template are never read
// (CLAUDE.md invariant 4), on a ReplicaSet no less than on a pod.
type RevisionContainer struct {
	Name  string
	Image string
	// Init marks an init container, the same distinction workload metadata
	// keeps: an init container's image changing is a revision changing.
	Init bool
}

// ReplicaSetInfo is the collected view of one ReplicaSet — one revision of a
// Deployment, with how many replicas it is currently carrying.
type ReplicaSetInfo struct {
	Namespace string
	// Workload is the owning Deployment. A ReplicaSet with any other controller
	// never reaches this view.
	Workload WorkloadRef
	// Name is the ReplicaSet's own name, which is what a `kubectl get rs` shows
	// and therefore what makes this record findable in the cluster.
	Name string
	// Revision is the Deployment controller's revision number. Nil when the
	// annotation is absent or unparseable — a real state, distinct from
	// revision zero, and one the agent reports rather than guesses at.
	Revision  *int64
	CreatedAt time.Time
	// DesiredReplicas is spec.replicas; Current and Ready are the status
	// counters. Together they say which revision is actually carrying traffic:
	// a rollout in progress has two revisions with non-zero counts, and a stuck
	// one has a revision with desired > 0 and ready == 0.
	DesiredReplicas int32
	CurrentReplicas int32
	ReadyReplicas   int32
	Containers      []RevisionContainer
}

// ReplicaSets returns the collected view of every ReplicaSet belonging to a
// Deployment that has admitted pods.
//
// Which Deployments those are is inherited rather than decided again: the set is
// read from the admitted pod index, so a workload excluded by a namespace
// filter or by an opt-out annotation on its namespace, itself or its pods is
// absent here too. One admission decision, one lifetime, no second source of
// truth — the property `podIndexEntry.info` already relies on, and the reason
// this needs no filter call of its own.
//
// A Deployment whose pods are all gone therefore has no revisions here, even
// while its ReplicaSets still exist. That is the same forgetting ADR 0018
// decided for the Go inventory: the snapshot is current state, and history is
// the backend's to accumulate.
func (w *PodWatcher) ReplicaSets() []ReplicaSetInfo {
	collected := w.collectedDeployments()
	if len(collected) == 0 {
		return nil
	}
	sets, err := w.rsLister.List(labels.Everything())
	if err != nil {
		return nil
	}

	out := make([]ReplicaSetInfo, 0, len(sets))
	for _, set := range sets {
		owner := metav1.GetControllerOf(set)
		if owner == nil || owner.Kind != "Deployment" {
			continue
		}
		if !collected[workloadKey{namespace: set.Namespace, name: owner.Name}] {
			continue
		}
		out = append(out, describeReplicaSet(set, owner.Name))
	}
	return out
}

type workloadKey struct {
	namespace string
	name      string
}

// collectedDeployments is the set of Deployments that currently have at least
// one admitted pod.
func (w *PodWatcher) collectedDeployments() map[workloadKey]bool {
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()
	out := make(map[workloadKey]bool)
	for _, entry := range w.index {
		if entry.workload.Kind != "Deployment" {
			continue
		}
		out[workloadKey{namespace: entry.namespace, name: entry.workload.Name}] = true
	}
	return out
}

func describeReplicaSet(set *appsv1.ReplicaSet, deployment string) ReplicaSetInfo {
	info := ReplicaSetInfo{
		Namespace:       set.Namespace,
		Workload:        WorkloadRef{Kind: "Deployment", Name: deployment},
		Name:            set.Name,
		Revision:        revisionOf(set),
		CreatedAt:       set.CreationTimestamp.UTC(),
		CurrentReplicas: set.Status.Replicas,
		ReadyReplicas:   set.Status.ReadyReplicas,
	}
	if set.Spec.Replicas != nil {
		info.DesiredReplicas = *set.Spec.Replicas
	}
	for _, c := range set.Spec.Template.Spec.InitContainers {
		info.Containers = append(info.Containers,
			RevisionContainer{Name: c.Name, Image: c.Image, Init: true})
	}
	for _, c := range set.Spec.Template.Spec.Containers {
		info.Containers = append(info.Containers,
			RevisionContainer{Name: c.Name, Image: c.Image})
	}
	return info
}

// revisionOf parses the Deployment controller's revision annotation. An absent
// or malformed value yields nil: the agent reports what it read, and inventing
// a number here would put two revisions under one identity.
func revisionOf(set *appsv1.ReplicaSet) *int64 {
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
