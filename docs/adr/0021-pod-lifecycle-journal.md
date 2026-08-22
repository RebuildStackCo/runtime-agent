# 0021. Pod lifecycle: the scheduler's reason goes where the shortfall already is, the cluster's removals become a journal

Date: 2026-08-22
Status: Accepted
Amends: 0012 §1
Amended by: 0022

Amends [ADR 0012](0012-payload-registry-and-provenance.md): the payload registry
gains `pod_disruptions`, and `workload_metadata` gains one field. ADR 0012's
decisions are otherwise unchanged, including §5 — replica counts are reported,
never interpreted — which this ADR follows rather than revises.

| Kind | Natural key | Delivery | Provenance |
|---|---|---|---|
| `pod_disruptions` | (window start, window length) | supersedes within its window | journal |

## Context

Two facts about a pod's life were being dropped, and they are not the same
shape.

**A replica that is not running.** ADR 0012 §5 already made this visible: a
`workload_metadata` record counts replicas and breaks them down by node, and
"unscheduled pods have no node, so `nodes` may sum to fewer than `replicas`;
that shortfall is the pending-scheduling signal". The count was there. The
*reason* was not — and without it, three very different situations look
identical: nothing in the cluster fits the pod, the pod is deliberately held by
a scheduling gate, or the scheduler failed on the pod's own spec. Only the first
is a capacity problem.

**A pod the cluster took away.** Preemption to make room for higher priority,
eviction by a node under memory or disk pressure, a taint manager draining a
node, the eviction API during a scale-down. Every one of these is this product's
subject — the cluster ran out of something — and none of them was collected.
Kubernetes records them on the `DisruptionTarget` condition with a reason and a
transition time, and the kubelet additionally sets `status.reason: Evicted`.

## Decision

**1. The scheduling reason goes into `workload_metadata`, not into a payload of
its own.** `PodScope` gains `unscheduled`: a map of scheduler reason to count.

This is the substance of the decision, not a placement detail. A separate
payload would have given the backend two independent ways to count unplaced
replicas — the `nodes` shortfall and a new record — and two counts of the same
thing diverge the moment a pod is between states. Putting the reason where the
count already lives makes them one signal by construction. It also keeps the
promise that this payload names no pods: reasons are counted, replicas are not
listed.

**2. The reasons are kept apart, not summed.** `Unschedulable`,
`SchedulingGated` and `SchedulerError` are reported under their own names.
Collapsing them into "pending" would turn a deliberate hold into a fabricated
capacity shortage — the agent reporting a conclusion, which ADR 0004 reserves
for the analysis.

**3. Disruptions are a journal payload, windowed like the restart journal.**
`pod_disruptions` holds one record per pod per hour-aligned window, one file per
window.

Windows here are a *delivery boundary rather than an aggregation*, and the
difference from ADR 0020 is worth stating: a restart count accumulates within
its window, while a pod is preempted exactly once and its record is complete on
arrival. What the window buys is the same bound ADR 0020 bought — a node
evicting forty pods writes one file, not forty — plus alignment with the usage
and restart windows of the same hour, so "the cluster evicted these pods" is
read next to what those pods were consuming.

**4. The record carries the node.** For a node-pressure eviction the node is the
subject: joined against `node_metadata` it turns "a pod was evicted" into "this
instance type ran out of memory". Without it the event says something happened
somewhere.

**5. The cluster's timestamp is used, not the observation instant.** Unlike a
restart — which Kubernetes does not timestamp, forcing ADR 0020 to baseline what
it did not see — a disruption carries `lastTransitionTime`. So a pod already
condemned when the agent starts lands in the window where it actually happened,
and the agent has no reason to invent a placement. The consequence is that a
window already delivered can gain a record; the payload supersedes under its
window key, so this is an upsert, not a duplicate.

**6. Reason, never message.** `DisruptionTarget.reason` and
`PodScheduled.reason` are drawn from Kubernetes' own vocabulary. The adjacent
`message` is free text: a scheduler message reads like *"0/12 nodes are
available: 3 node(s) had untolerated taint {dedicated: payments-gpu}"*, naming
nodes, taints, and other teams' arrangements. Same distinction, same answer, as
the build settings of ADR 0019 and the termination reasons of ADR 0020. Reasons
outside the known set are counted under `other` rather than passed through,
because the vocabulary is upstream's to extend and a payload whose keys a
Kubernetes release chooses has unbounded shape.

**7. A pod that failed on its own is not a disruption.** `phase: Failed` with
`reason: Evicted` is the cluster removing a pod; `phase: Failed` because a
container exited 1 is the workload failing, and it belongs to
`container_restarts`. The distinction is enforced in code and has a test named
for it.

## Consequences

**Easier.** "Why is this workload short of replicas" is answerable from one
payload, and answerable *correctly* — a gated pod no longer reads as a capacity
shortage. Preemption and node-pressure eviction become visible for the first
time, with the node and the moment, alongside the usage window that shows what
the cluster was doing when it ran out.

**Harder / given up.** How *long* a pod stayed unschedulable is not reported.
`unscheduled` is a snapshot count, taken at each flush like the rest of
`workload_metadata`, so a pod that was unschedulable for fifty minutes between
two flushes is invisible. Duration would need its own accumulator, and the
question it answers — "how much scheduling pressure is there over time" — is one
the backend can approximate from the sequence of snapshots.

Disruptions of pods the customer's filters exclude are not collected at all,
which means a namespace-filtered pod being evicted is not reported even though
the *node* it was evicted from may host collected workloads. That is the
filter-early rule working as designed (CLAUDE.md invariant 4), and the node's
own pressure remains visible through the usage payloads of the workloads that
are collected.

**Verified against a real cluster rather than asserted.** The end-to-end test
stages an actual preemption — a low-priority workload sized to the node's *free*
CPU, then a high-priority one asking for the same room — and asserts the record
that results. The first attempt sized the pods against allocatable CPU instead
of free CPU and produced no preemption at all; the correction is in the test, in
a comment that says why.

**Not addressed here.** Object-level history — ReplicaSet revisions and Job
timings — is the remaining journal slice. Unlike this one it needs facts from
objects other than pods, which is where the informer set actually grows.

This ADR records a decision implemented in the same pull request, per the
process in [`README.md`](README.md).
