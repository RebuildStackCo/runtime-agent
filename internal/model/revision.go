package model

import "time"

// ReplicaSetInfo is the collected view of one ReplicaSet — one revision of the
// workload that controls it, with how many replicas it is currently carrying.
type ReplicaSetInfo struct {
	Namespace string
	// Workload is the controller that owns the set: a Deployment, or any custom
	// resource that manages ReplicaSets (ADR 0049). A ReplicaSet with no
	// controller never reaches this view.
	Workload WorkloadRef
	// Name is the ReplicaSet's own name, which is what a `kubectl get rs` shows
	// and therefore what makes this record findable in the cluster.
	Name string
	// Revision is the Deployment controller's revision number. Nil when the
	// annotation is absent or unparseable, and nil for every other controller,
	// which numbers its revisions under a key of its own (ADR 0049 §3).
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
