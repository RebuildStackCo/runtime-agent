// Package revisions reduces the controller's ReplicaSet view to the deployment
// revisions payload: which builds of a Deployment exist, when each appeared, and
// how many replicas each is carrying.
//
// It is reduction, not judgment (CLAUDE.md: the agent's intelligence is data
// reduction). Nothing here decides that a rollout is stuck, that a revision is
// old, or that a change caused anything; those are backend renderings of these
// facts. The package holds no state: every snapshot is derived from the live
// ReplicaSet view, so it is loss-harmless by construction (ADR 0003).
package revisions

import (
	"sort"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

// Container is one container of a revision's pod template.
type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// Init marks an init container. An init container's image changing is a
	// revision changing, so the distinction must survive.
	Init bool `json:"init,omitempty"`
}

// Replicas is how many pods a revision is meant to carry and how many it
// actually has. The three together are what distinguish a finished rollout from
// one in progress from one that is stuck: a rollout in flight has two revisions
// with non-zero counts, and a stuck one has a revision whose `desired` is
// positive and whose `ready` is not.
//
// The agent reports the three numbers and names none of those states. Which of
// them a reader is looking at is a rendering, and it belongs to whoever has the
// history to compare against (ADR 0004).
type Replicas struct {
	Desired int32 `json:"desired"`
	Current int32 `json:"current"`
	Ready   int32 `json:"ready"`
}

// Record is one revision of one Deployment: the ReplicaSet the Deployment
// controller created for it, what it runs, and what it is carrying.
//
// This is the payload's whole purpose: a usage change with no attributable
// cause is an observation, and a usage change next to "revision 47 appeared two
// minutes earlier" is a finding. The agent supplies the second half and joins
// nothing — the join is the backend's, against `workload_metadata` and the
// usage windows under the same workload key.
type Record struct {
	Namespace string                `json:"namespace"`
	Workload  collector.WorkloadRef `json:"workload"`
	// Name is the ReplicaSet's own name, so a reader can find the object this
	// record describes in their own cluster.
	Name string `json:"name"`
	// Revision is the Deployment controller's revision number, absent when the
	// annotation was missing or unparseable. Absent is a state, not a zero.
	Revision *int64 `json:"revision,omitempty"`
	// CreatedAt is when the ReplicaSet was created, which is when this revision
	// first existed — **not** when it became active. A rollback reuses the
	// earlier ReplicaSet and gives it a new revision number, so a rolled-back
	// revision carries the creation instant of its original rollout. The agent
	// reports the object's fact and draws no conclusion (ADR 0004, ADR 0030).
	CreatedAt  time.Time   `json:"created_at"`
	Replicas   Replicas    `json:"replicas"`
	Containers []Container `json:"containers,omitempty"`
}

// Aggregate reduces the ReplicaSet view to one record per revision, ordered by
// namespace, then workload, then revision, so the payload bytes are
// deterministic — the golden contract (docs/development.md).
//
// Only ReplicaSets of Deployments with admitted pods reach this function; the
// identity of an excluded workload never appears here, in keeping with
// CLAUDE.md invariant 6.
func Aggregate(sets []collector.ReplicaSetInfo) []Record {
	out := make([]Record, 0, len(sets))
	for _, set := range sets {
		rec := Record{
			Namespace: set.Namespace,
			Workload:  set.Workload,
			Name:      set.Name,
			Revision:  set.Revision,
			CreatedAt: set.CreatedAt.UTC(),
			Replicas: Replicas{
				Desired: set.DesiredReplicas,
				Current: set.CurrentReplicas,
				Ready:   set.ReadyReplicas,
			},
		}
		for _, c := range set.Containers {
			rec.Containers = append(rec.Containers,
				Container{Name: c.Name, Image: c.Image, Init: c.Init})
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Workload.Name != b.Workload.Name {
			return a.Workload.Name < b.Workload.Name
		}
		// A missing revision sorts before any numbered one, so the ordering is
		// total even on a ReplicaSet the Deployment controller never annotated.
		ai, bi := revisionOrZero(a.Revision), revisionOrZero(b.Revision)
		if ai != bi {
			return ai < bi
		}
		return a.Name < b.Name
	})
	return out
}

func revisionOrZero(r *int64) int64 {
	if r == nil {
		return -1
	}
	return *r
}
