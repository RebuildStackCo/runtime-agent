# 0031. Placement constraints are collected, reduced rather than copied

Date: 2026-08-24

Status: Accepted

Adds a `placement` block to the pod scope of `workload_metadata`. No new payload
kind, no new informer, no new RBAC.

## Context

The agent can say what a workload consumes, what it asked for, and which machine
it got. It cannot say why it is on that machine and not a cheaper one. That last
question is where the resource units become actionable: a workload whose replicas
each sit on a large node may be there because it needs the size, or because a
required anti-affinity on `kubernetes.io/hostname` forbids two replicas sharing a
node — and those two situations lead to opposite conclusions from identical usage
numbers.

Every fact needed to tell them apart is in the pod spec, which the agent already
reads in full to collect requests, limits and ports. Nine fields describe where a
pod may run and what it costs to move it, and none of them were collected.

## Decision

**1. Nine fields of the pod spec are collected**: `nodeSelector`, the three
`affinity` blocks, `topologySpreadConstraints`, `tolerations`,
`priorityClassName`, `terminationGracePeriodSeconds`, `hostNetwork` and
`schedulerName`.

They are what stops a workload from moving. Required anti-affinity on hostname
means one replica per node whatever capacity exists elsewhere; `DoNotSchedule`
across zones means paying for every zone the workload spans; a toleration is
usually what lets a pod onto the hardware a cluster fenced off with a taint;
`terminationGracePeriodSeconds` is how long draining a node waits per pod, which
is the cost of the consolidation itself. `priorityClassName` also explains
preemptions the agent already reports in `pod_disruptions`.

**2. The block lives in the pod scope of `workload_metadata`, not in a new
kind.** Placement is a property of the pod, not of the container, so it nests
where the other pod facts nest (ADR 0014) and repeats across a pod's containers
exactly as the replica counts do — the repetition ADR 0014 accepted in exchange
for the scope being visible in the payload itself.

A separate kind would have needed a second key for the same subject and would
have split one question across two files.

**3. The record key already separates a rollout's revisions.** The block is taken
from the first pod seen under the key, the way `image`, `resources` and `ports`
already are. That is safe here because the key carries the image digest: a
rollout that tightens placement produces one record per build, each with its own
constraints, instead of whichever pod was seen first speaking for both.

**4. The affinity structures are reduced, not copied.** Node affinity is
flattened to a list of (key, operator, values, required, weight); pod affinity
and anti-affinity keep the topology key, whether the term is required, and the
weight; spread constraints keep the topology key, skew, `whenUnsatisfiable` and
`minDomains`. The label selectors inside the pod affinity and spread terms are
dropped.

This is deliberately lossy, and the loss is stated rather than hidden: node
affinity's terms are OR'd with each other and AND'd within themselves, and
flattening loses that structure. What survives answers "what is this workload
pinned on, and how hard" — which is the question the payload exists for.
Replaying the scheduler's decision from this payload is not possible, and is not
something the agent offers. A faithful copy would mean carrying an arbitrarily
nested selector into every record of every container, to support an inference the
product does not make.

Required and preferred are kept apart. Only a required term can leave a pod
unschedulable, so folding them together would erase the distinction the field
exists for.

**5. Operator-written strings are bounded, and what does not fit is dropped
whole and counted.** This is the second place after build settings where free-form
strings written by whoever wrote the manifest enter the agent, so it takes ADR
0019's discipline unchanged: a value over 128 bytes means the string is not what
this reduction assumes, and the term carrying it is dropped rather than
truncated, because a prefix of an unexpected string is still an unexpected
string. Term lists and value lists have counts bounded the same way.

A value list past its bound is dropped whole while its key and operator survive.
A partial list would read as a complete one; the key is the half that says what
the workload is pinned on.

Two counters — `placement_values_dropped` and `placement_terms_dropped` — reach
the coverage report. They are aggregate: what was dropped is counted, never
named (CLAUDE.md invariant 6). Both sit at zero on an ordinary cluster, and a
number in either is the signal that a customer's manifests contain something this
reduction does not carry.

**6. Cluster-wide defaults are not collected, because they are not facts about
the workload.** The `DefaultTolerationSeconds` admission plugin puts
`node.kubernetes.io/not-ready` and `node.kubernetes.io/unreachable` at 300
seconds on every pod in the cluster; carrying them would repeat two identical
entries on every record and state nothing. The match is exact on the seconds, so
a pod that tuned one — how fast it leaves a failing node — keeps it. The same
applies to `default-scheduler` and to a 30-second grace period: only the
deviation is a fact.

**7. Three things next to these fields are deliberately not read.** `env`, `args`
and `command` sit in the same pod spec and are never read (CLAUDE.md invariant
4), which is checked here on the encoded bytes rather than on the shape of the
struct, so a field added later that carries part of the spec through fails a test
rather than a customer's cluster. `volumes` are not read either: a zonal volume
does pin a pod to a zone, but resolving it runs through the StorageClass, which
needs RBAC the agent does not have, and the volume list references Secrets and
ConfigMaps by name.

And one fact turns out not to be observable at all: a pod pinned by hand through
`spec.nodeName` is indistinguishable after scheduling from one the scheduler
placed, because every scheduled pod carries the field. It is not reported rather
than guessed at.

## Consequences

**Easier.** "This workload cannot be consolidated" becomes a statement supported
by the manifest rather than inferred from the shape of its usage. The three
payloads now compose: `workload_metadata` says what was asked for, `node_metadata`
says what machine answered, and the placement block says what forbids a different
answer. Cross-zone spread and hostname anti-affinity — the two constraints that
most often make a fleet larger than its usage suggests — are visible directly.

**Harder / given up.** The reduction cannot be inverted. A consumer that wants to
know whether a specific node satisfies a specific pod's affinity cannot compute it
from this payload, and should not try; that is a question for the cluster, not for
a telemetry record.

Payload size grows only for constrained workloads. Every field is `omitempty` and
the block itself is `omitzero`, so a pod with no constraints — after the
cluster-wide defaults above are excluded — contributes no bytes at all.

**Not changed.** No RBAC: the pod spec was already read in full. No new informer,
no new payload kind, no new file in the spool. `workload_metadata` changes shape,
which is a protocol change and shows as changed golden bytes.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
