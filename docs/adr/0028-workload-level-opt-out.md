# 0028. The opt-out annotation works on the workload, and the workload step fails open

Date: 2026-08-23
Status: Accepted

## Context

`security.md` §11 offers the customer two ways to exclude something from
collection without editing the agent's configuration: the annotation
`rebuildstack.co/collect: "false"` on a Namespace, or the same annotation on a
Pod. The second one is where the trouble is.

**Nobody annotates pods.** Pods are created by controllers, so excluding a
workload means putting the annotation in the controller's pod template — and a
pod template's annotations are part of the template hash. Writing one rolls
every replica. The only documented way to opt a Deployment out of telemetry is
to restart it in production. For a StatefulSet with slow startup, or a workload
under a change freeze, that is not a control anyone will use.

**And the intuitive place does nothing.** An annotation on the Deployment,
StatefulSet, DaemonSet or CronJob object itself is not inherited by pods and was
never consulted. A customer who writes it there gets silence: no error, no
warning, and full collection continuing. This came up while planning the
`job_runs` slice, where the same question — "which Jobs are excluded" — had no
answer that agreed with the pods.

There is a second question underneath, and it is the one that decides the shape.
The agent can read Deployments, StatefulSets, DaemonSets, CronJobs, Jobs and
ReplicaSets; it already has RBAC for all of them. It cannot read an Argo
Rollout, a Knative Revision, or any operator's custom resource — doing so would
need RBAC on arbitrary custom resources, which the product does not ask for and
will not start asking for. So for some pods the workload-level annotation simply
cannot be checked, and something has to be decided about them.

## Decision

**1. The annotation works on the workload object.** The filter gains a step
between the namespace and the pod: the controller that ultimately manages the
pod — Deployment, StatefulSet, DaemonSet, CronJob, or a bare Job or ReplicaSet —
is read through the same owner chain `resolveWorkload` already walks, and its
own annotations are honored.

Explicitly **not** the controller's pod template. The whole point is a control
that does not touch the template, because touching the template is what forces
the rollout.

The three existing controls are unchanged; this is a fourth, and it is the one
that matches what a customer expects to be able to do.

**2. The workload step is attributed before the pod step.** A workload that
opted out usually leaves no annotation on its pods at all, so if both matched,
naming the pod would point the customer at an object they never wrote on. The
evaluation order is namespace filter → namespace annotation → workload
annotation → pod annotation, and the reported reason is the first match.

**3. The workload step fails open.** A pod whose controller cannot be read is
**collected**, and the reason it could not be read is counted.

This is the opposite of ADR 0015's discipline for the node's scan scope, and
deliberately so, because the question is different. There, the node asked "am I
allowed to look at this at all", an authoritative answer existed, and proceeding
without it would have meant collecting something the customer excluded. Here the
question is "did the customer write an opt-out somewhere I cannot see", and the
honest answer is almost always no: an annotation is a rare, deliberate act, and
this product is opt-out by construction — an empty allow list collects
everything.

Failing closed would invert that. Every cluster running Argo Rollouts, Knative,
Spark, KubeVirt or any in-house operator would silently collect nothing from
those workloads, and the product would appear broken on exactly the clusters
that are most worth measuring. The customer also loses nothing by this choice:
the namespace annotation and the pod annotation both still work on a
CRD-managed workload.

**4. The blind spot is counted, in two separate numbers.** `workload_unknown_kind`
counts pods whose controller is a kind the agent does not read;
`workload_not_cached` counts pods whose controller is a known kind that was not
in the informer cache.

They are apart because they mean different things. The first is a standing
property of the cluster — "you run an operator we do not read; use the namespace
or the pod annotation for those" — and it is a fact the customer is entitled to
see rather than discover. The second is transient, a resync race or an object
deleted between the pod event and the lookup, and a persistently non-zero value
is a defect in the agent, not a property of the cluster. Folded together, the
defect would hide inside the property.

Both are aggregate counts. No identity of a pod or a workload appears
(CLAUDE.md invariant 6), and both count admissions rather than exclusions —
the number is the size of the blind spot, which is only meaningful against the
pods that *were* collected.

**5. `PodFilter` becomes `Filter`.** It decides about pods today and about Jobs
in the next slice, and a type named for one of the things it gates is how
documentation drifts from behaviour. Mechanical rename, no behaviour attached.

## Consequences

**Easier.** Excluding a workload is one annotation on the object a customer
already thinks of as the workload, with no rollout and no configuration change.
The coverage report gains the one number that says where this control does not
reach, so its limit is disclosed rather than discovered. And `job_runs` has a
filtering rule that can agree with the pods, which is what the next slice needs.

**Harder / given up.** Four more informers — Deployments, StatefulSets,
DaemonSets, CronJobs — on low-cardinality objects, with no new RBAC and no new
API call pattern; they are watches on objects the controller is already
permitted to list. Pod admission now depends on the owner-chain caches being
synced, which `Run` waits for before delivering pod events.

The workload-level control does not reach CRD-managed workloads, and cannot
without RBAC the product refuses to ask for. That is a real limit, it is
disclosed in `security.md` §11 and counted in the coverage report, and the two
other controls cover the same ground less conveniently.

**Not changed.** What is collected about an admitted pod, every payload, and the
namespace and pod annotations themselves. A cluster where nobody annotates a
workload behaves exactly as before.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
