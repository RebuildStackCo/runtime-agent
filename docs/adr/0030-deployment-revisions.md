# 0030. Deployment revisions ship as current state, scoped by the pods already admitted

Date: 2026-08-23
Status: Accepted

Adds the payload kind `deployment_revisions`. Nothing existing changes shape.

## Context

A usage change with no attributable cause is an observation, not a finding. The
agent can say that a workload's CPU doubled at 14:03; it cannot say that the
workload started running a different build at 14:01. Both halves are in the
cluster, the agent already watches the objects that hold the second, and it
reports only the first.

The Deployment controller records its history as ReplicaSets: one per revision,
each annotated with its number, each carrying the pod template that revision
runs and the replica counts it currently has. The agent has a ReplicaSet informer
running — it resolves owner chains with it — and RBAC to read them. Nothing about
this fact is expensive to collect; it was simply never collected.

## Decision

**1. `deployment_revisions` is a superseding snapshot, not a journal.** Kind
`deployment_revisions`, source `structural`, natural key the payload kind itself,
one per cluster — the same shape as `workload_metadata` and `node_metadata`.

A journal would be the wrong model. Every fact is already on the ReplicaSet
object, so a snapshot derived from the lister holds no state and is
loss-harmless by construction (ADR 0003). And the live set is small:
`revisionHistoryLimit` defaults to ten, so a Deployment's whole visible history
fits in the snapshot.

History beyond what the cluster keeps is the backend's to accumulate across
snapshots. That is ADR 0018's decision for `go_inventory`, and it applies
unchanged: a revision the cluster garbage-collected is not a revision the agent
should keep alive.

**2. One record per revision**, carrying the namespace, the owning Deployment,
the ReplicaSet's own name, the revision number, when it was created, the desired,
current and ready replica counts, and each container's name and image — init
containers included and marked, because an init container's image changing is a
revision changing.

The three replica counts are what distinguish a finished rollout from one in
progress from one that is stuck: a rollout in flight has two revisions with
non-zero counts, and a stuck one has a revision whose desired is positive and
whose ready is not. The agent reports the three numbers and names none of those
states — which one a reader is looking at needs history to compare against, and
that is a backend rendering (ADR 0004).

The image is the point of the record. "Revision 47 is this build" is what turns
a usage change into an attributable one.

**3. Exactly one field is read from the pod template: the container image.** The
`env`, `args` and `command` beside it in the same template are never read, on a
ReplicaSet no less than on a pod (CLAUDE.md invariant 4). This is checked on the
bytes rather than on the shape, so a field added later that carries the template
through fails a test rather than a customer's cluster.

**4. The revision set is inherited from the admitted pod index, not decided
again.** A Deployment appears here only while it has admitted pods. There is no
filter call in this path.

That is deliberate and it is the stronger construction. A workload excluded by a
namespace filter, or by an opt-out annotation on its namespace, itself or its
pods, has no admitted pods, so it cannot reach this payload — the same
by-construction property ADR 0025 relied on for the targeting reply, and the same
"one admission decision, one lifetime, no second source of truth" the pod index
already provides. A second filter call would be a second place to keep in step.

The consequence is that a Deployment scaled to zero has no revisions here, even
while its ReplicaSets exist. Accepted, and the same forgetting ADR 0018 chose.

**5. Deployments only.** StatefulSet and DaemonSet revisions live in
`controllerrevisions`, which the agent has no RBAC for and will not request for
this. The kind's name says so rather than implying a generality it lacks.

**6. Two facts are reported honestly rather than corrected.**

`created_at` is when the ReplicaSet was created, which is when the revision first
existed — **not** when it became active. A rollback reuses the earlier ReplicaSet
and gives it a new revision number, so a rolled-back revision carries the
creation instant of its original rollout. Deriving an activation time would mean
inferring it from replica counts the agent samples once a minute, which is a
guess dressed as a fact.

`image` is the template's reference, usually a tag, while `go_build` is keyed by
image digest. The join from a revision to its build therefore runs through pods,
which carry the digest, and works only while those pods exist. Resolving the tag
ourselves would mean talking to a registry, which the agent does not do.

**7. Revisions share the metadata flush's capture instant.** They are joined
against `workload_metadata` under the same workload key, and two capture times
would let a consumer pair a revision with a shape from a different moment.

## Consequences

**Easier.** A usage change can be attributed. The rollout in progress at the
moment a window was measured is visible, with which build each revision runs, so
"this got more expensive" can become "this got more expensive when it started
running that". None of that inference is in the agent; what the agent adds is the
half of the join that was missing.

**Harder / given up.** The activation time of a revision is not reported and
cannot be derived from this payload alone. A backend accumulating snapshots can
bound it — the revision was not carrying replicas in one snapshot and was in the
next — which is the right place for it, since that is exactly the history the
agent deliberately does not keep.

Payload size grows with revision count: at the default `revisionHistoryLimit` of
ten, a cluster with two hundred Deployments produces on the order of two thousand
records per snapshot, most of them at zero replicas. Bounded by a cluster
setting, and the field set per record is small.

**Not changed.** No RBAC, no new informer — the ReplicaSet watch was already
running for owner resolution. No existing payload's shape. A cluster whose
Deployments have no admitted pods writes an empty record set rather than nothing,
because a superseding snapshot with no records is a truthful statement that
nothing is collected, unlike a journal where silence means a quiet window.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
