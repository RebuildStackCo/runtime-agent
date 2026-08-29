# 0032. What bounds a workload from outside its own spec, and the first widening of read access

Date: 2026-08-24

Status: Accepted
Amends: 0031 §7
Amended by: 0033, 0048, 0051

Adds the payload kinds `workload_policy` and `cluster_policy`. Adds six resources
to the ClusterRole — the first time the agent's read access has widened since it
was first written. Amends ADR 0031 §7, which declined `spec.volumes` wholesale.

## Context

The agent now reports what a workload consumes, what it asked for, which machine
it got, and what in its own spec keeps it there (ADR 0031). Everything still
missing is in objects the agent has never read.

Three of them change what may be concluded rather than merely adding detail. A
PodDisruptionBudget can forbid eviction outright, so a node covered by one cannot
be drained however much spare capacity exists — that is not an expensive
consolidation, it is an impossible one. A HorizontalPodAutoscaler targeting CPU
utilization targets a percentage *of the request*, so a recommendation to lower
requests silently changes how the workload scales. A LimitRange supplies the
request a container never declared, which means a number already in
`workload_metadata` may be a team's decision or a namespace's default, and the
payload cannot tell those apart today.

Reading them means widening the ClusterRole. That is the cost this ADR is mostly
about.

## Decision

**1. Six resources are added, all read-only**: `poddisruptionbudgets`,
`horizontalpodautoscalers`, `limitranges`, `resourcequotas`,
`persistentvolumeclaims`, `priorityclasses` and `storageclasses` — with
`get`/`list`/`watch` and nothing else. No verb that writes is added, and the
statement that the controller's access is read-only stands without qualification.

The grant is wider than the filter and that is worth stating plainly: namespace
allow/deny lists live in agent code, so in a cluster-wide installation the
ClusterRole permits reading these objects in every namespace, including excluded
ones. Filtering is a promise the code keeps; RBAC is a capability. The
namespace-scoped installation mode remains available and is now narrower in one
more respect — `priorityclasses` and `storageclasses` are cluster-scoped, so that
mode cannot read them at all.

**2. `workload_policy` carries what bounds one workload**: the budgets covering
it, the autoscalers driving it, and the volume claims its pods mount. Superseding
snapshot, fixed key, `structural` source.

Budgets are plural because selectors overlap and the binding budget is the
stricter one at a given replica count — a determination that needs a moment in
time, so the agent reports both and decides nothing (ADR 0004). An autoscaler's
metric target and its target *type* always travel together: `80` means nothing
without "percentage of the request" attached.

A workload that nothing constrains produces no record. In a superseding snapshot
absence is a statement — "nothing bounds this" — unlike a journal, where silence
would mean a quiet window.

**3. `cluster_policy` carries namespace policy and the two catalogs.** One kind
rather than three: the subject is the cluster's policy configuration, and the
scopes inside it stay visible in the structure instead of being flattened, which
is ADR 0014's nesting principle applied above the pod.

Namespaces are those with admitted pods, so an excluded namespace is never named.
The catalogs are **not** filtered: a PriorityClass and a StorageClass are cluster
infrastructure rather than a customer workload, the same standing on which the
agent already reports the entire node fleet. Their names are what the placement
block and the claims point into.

**4. Two joins, two mechanisms, and the difference is not cosmetic.** An
autoscaler names its workload in `scaleTargetRef`, so that join is exact. A
budget names *pods* by label selector, so its join runs through the admitted pod
index — and a budget whose pods are all excluded therefore attaches to nothing
and is never named. The identity discipline holds by construction rather than by
a check that could be forgotten (CLAUDE.md invariant 6).

**5. Scope is inherited from the admitted pod index**, as in ADR 0030. No second
admission path exists in this payload, so nothing can drift out of step with the
filters.

**6. `spec.volumes` is read for `persistentVolumeClaim` entries only, and this
amends ADR 0031 §7.** That ADR declined the field wholesale, giving as its reason
that the volume list references Secrets and ConfigMaps by name. The reason still
holds and is unchanged — it simply does not reach the subset now read. Only
entries whose source is a claim are looked at; a Secret, ConfigMap or projected
volume in the same list is skipped without its name being touched, which is
checked on the encoded bytes.

What the subset buys is a placement constraint no pod-spec field states: a bound
claim on a zonal volume pins its pod to that zone for as long as the claim
exists, and `volumeBindingMode` on the class decides whether that binding happens
before or after scheduling.

**7. A StorageClass's `parameters` are not read.** It is the one field in this
whole payload that carries operator-written provider configuration — endpoints,
resource groups, key identifiers. What is read is how the class behaves, not how
it is wired.

**8. Quota vocabulary is kept as Kubernetes writes it.** A ResourceQuota's keys
are open-ended (`requests.cpu`, `pods`, `count/deployments.apps`), so reducing
them to a cpu/memory pair would drop the rest silently. Keys and quantities are
kept as written and bounded like every other external string (ADR 0031 §5). The
effective ceiling is read from `status.hard` rather than `spec.hard`, so a quota
edited but not yet reconciled is not reported as already in force.

## Consequences

**Easier.** The three conclusions that were unsafe become safe. "This node can be
consolidated" can account for a budget that forbids it. "This workload is
over-provisioned" can account for an autoscaler whose behavior depends on the
request. "This team asked for too little" can distinguish a choice from a
namespace default. And a workload pinned to a zone by its storage is visible as
such rather than as an unexplained placement.

**Harder / given up.** The ClusterRole is longer, and a security reviewer reads a
longer list. That cost is real and is the reason this ADR exists rather than the
change being folded into a payload slice. The namespace-scoped installation mode
loses two more capabilities.

A budget's own selector is not reported, only the workloads it resolved to. A
consumer cannot re-evaluate the selector against pods the agent did not admit,
which is the intended asymmetry.

**Not changed.** No write verb anywhere. No new informer beyond the six listers
on the factory already running. No existing payload's shape: `workload_metadata`,
`deployment_revisions` and the journals are untouched, and the two new kinds are
additions to the registry rather than edits to it.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
