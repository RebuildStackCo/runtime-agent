# 0049. Revisions belong to whatever controls the ReplicaSet, and the payload is renamed for it

Date: 2026-08-29

Status: Accepted

Amends: 0030 §1–2

Widens `deployment_revisions` to every workload that manages ReplicaSets and
renames it `workload_revisions`. No new RBAC, no new field, no new payload kind.

## Context

ADR 0030 scoped revisions to Deployments, and at the time that was the whole of
what manages ReplicaSets in a cluster this agent could see. It is not. Argo
Rollouts is widely deployed, and a `Rollout` creates and scales ordinary
ReplicaSets exactly as a Deployment does — the agent already lists them, already
resolves their pods, and already reports `workload_kind: Rollout` (ADR 0048 §2).
The only thing standing between those sets and the payload was a kind check.

Measured on kind (Kubernetes 1.36.1) against Argo Rollouts v1.10.0, mid-canary
with `setWeight: 25` on four replicas:

    payments-7bd58f4db4  rev 2  desired 1  current 1  ready 1  pause:3.10
    payments-f47dc5486   rev 1  desired 3  current 3  ready 3  pause:3.9

That is the shape ADR 0030 §2 already describes — two revisions with non-zero
counts is a rollout in flight, and the split is the weight it is running at. The
record needs no new field to say it; it was simply never emitted.

`revisionHistoryLimit` defaults to 10 on a Rollout as it does on a Deployment,
so the bound ADR 0030 leaned on to keep the snapshot small holds unchanged. With
the limit set to 3 the cluster retained five sets: three historical, the stable
one, and the one in flight.

Two things this does not reach, and both are worth stating so the widening is
not read as more than it is. A `Rollout` paused at a step and a `Rollout`
degraded look identical from the ReplicaSets — `status.phase` and
`pauseConditions` are on the custom resource, which the agent does not read. And
StatefulSets and DaemonSets are still absent: their revisions live in
`controllerrevisions`, which ADR 0030 declined and this does not revisit.

## Decision

**1. Any ReplicaSet whose controller is a collected workload is reported**, and
the kind check goes. What made a Deployment special was that the agent knew the
name of its revision annotation, not anything about the object graph. Scope is
still inherited from the admitted pod index, so an excluded workload is absent
here exactly as before, and the namespace and pod opt-outs reach these workloads
even where the workload-level one cannot (`docs/security.md` §11).

**2. A ReplicaSet with no controller is not a revision.** A bare set is the
workload rather than one version of one, and it has no history to report; its
pods already resolve to the set itself. It is skipped rather than reported as a
revision of nothing.

**3. The revision number is read for Deployments and no one else.** Every
controller numbers its revisions under a key of its own —
`rollout.argoproj.io/revision` for Argo — and the agent reports `revision:
null` for all of them rather than learning vendor keys.

No finding needs the number. Deploy frequency needs the ordering, which
`created_at` gives; which revision is live needs the replica counts, which ship;
"this revision is that build" needs the image and the instant, which ship. A
table of vendor annotation keys would be a second list to keep in step with
other people's releases, bought for a field nothing reads. Records with no
number sort by age, so the payload stays deterministic.

**4. The kind is renamed `deployment_revisions` → `workload_revisions`**, and
the spool file with it. The old name would be false the moment the first Argo
record appeared, and a payload kind whose name contradicts its contents is the
defect ADR 0022 exists to prevent. The rename costs a registry row, a golden and
three documents today; after a backend exists it is a protocol migration. ADR
0019 renamed `go_dependencies` → `go_build` on the same reasoning.

## Consequences

**Easier.** The regression watch — CPU per replica across an image change — now
covers Argo-managed workloads, which was one of the three things such a workload
lost. Deploy frequency and rollout-in-flight follow for every ReplicaSet-based
controller at once, including operators nobody here has heard of, because
nothing in the reduction names a vendor.

**Harder / given up.** A consumer can no longer assume a revision number exists.
It was already optional — an unannotated ReplicaSet yielded null before this —
but it was rare, and it now happens for a whole class of workload. The backend
contract states it.

The payload can now carry a workload kind that is not a Kubernetes built-in, so
`workload.kind` is a string from a CRD rather than from a closed set. It is a
kind name, not free text, and it is already shipped in `workload_metadata` for
the same workloads (ADR 0048 §2), so this exposes nothing new.

**Not changed.** No new RBAC — ReplicaSets were already granted and read. No new
field, no new payload kind, no change to the natural key or the delivery
discipline. StatefulSet and DaemonSet revisions remain uncollected, and the
custom resource itself remains unread: what this decision uses is the object the
agent was already watching.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
