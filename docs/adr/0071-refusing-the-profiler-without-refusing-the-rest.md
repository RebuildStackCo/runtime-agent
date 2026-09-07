# 0071. A workload can refuse the profiler without refusing everything else

Date: 2026-09-07

Status: Accepted

Amends: 0025, 0028, 0057, 0058

Adds a fifth customer control, `rebuildstack.co/profile: "false"`, which stops
both profiling paths and the endpoint confirmation with them. Adds two coverage
counters, one log line per capture, and a disclosure block to the chart's
`NOTES.txt`. No new RBAC, no new read, no new capability, no change to what a
profiled workload ships.

## Context

`security.md` §11 offers four controls and says they are the complete list. All
four are the same control at four scopes: **do not collect this**. Which means
the only way for a customer to not be profiled is to not be collected — to give
up every rollup, every build fact and every restart count of a service in order
to decline one ten-second capture of it. That is not a trade anyone should be
asked to make, and it is the whole of the gap.

Pulling stays on by default. That was argued in
[ADR 0058](0058-the-pull-starts-a-profiler-and-the-binary-says-whose-code-it-is.md):
what it costs is bounded by construction rather than by a setting, a refusal is
respected for six hours and never fought, and whoever installed a node DaemonSet
with `CAP_SYS_PTRACE` has already made the larger decision. Nothing here
reopens it. What was never settled is the customer who accepts the default for
their cluster and wants one service out of it.

Two smaller things sit beside the gap. The chart's `NOTES.txt` warns about
running as root, about `hostPID`, and about a CNI that accepts a NetworkPolicy
without enforcing it — and says nothing about the agent connecting to the
customer's workloads and starting profilers inside them. And the only record
that a capture happened is a counter in `collection_coverage`, a payload that
today reaches nobody, because the transport of ADR 0008 is unbuilt. An SRE
reading an unexplained ten-second CPU step in a service has nowhere to look.

[ADR 0025](0025-node-config-holds-only-what-the-node-enforces.md) removed the
configured eligible set and recorded the consequence: "there is no longer a way
to profile *less* than what is collected", accepted because the knob that would
have served was one nobody would set, and because "adding a narrowing filter
later is additive and non-breaking". This is that addition, and it is an
annotation rather than a second namespace list for exactly the reason that ADR
gave for deleting the first one.

## Decision

**1. A fifth control, and the model it belongs to is unchanged.**
`rebuildstack.co/profile: "false"` excludes a workload from profiling and from
nothing else. §11 now says five.

The count is worth stating rather than quietly editing, because a control that
*enabled* something would be the change that breaks this product's model: a
customer would have to hold two mental shapes at once — what is on unless I
refuse, and what is off unless I ask. All five controls are refusals. The
sentence a customer has to carry is the same sentence it was: the agent
collects, and profiles, unless you say not to. A fifth way of saying not to does
not add a second model; it adds one more scope to the only one there is.

**2. It stops profiling entirely — both paths, and the endpoint confirmation
with them.** The annotation removes the workload from the targets the controller
hands a node's eBPF profiler, from the pprof candidate funnel, and therefore
from the pull.

The intent a customer expresses is about their process, not about our
mechanisms. An annotation that stopped the eBPF capture and left the pprof pull
running, or the reverse, would be a promise that cannot be said in one sentence,
and both paths are decided in the same place on the controller, so the narrower
reading buys nothing.

Confirmation goes with them for the same reason. A `GET /debug/pprof/` costs the
workload nothing measurable, and ADR 0057 §4 is right that there is no
meaningful sense in which the workload pays for it. But "the agent does not
connect to a workload you marked" is a promise that can be checked and defended;
"does not connect, except one request that costs nothing" is a promise that has
to be explained at an incident review. What the exception would buy is one line
in a report about a service the customer has already declined to profile.

**3. The shape is the collection opt-out's, not a second one.** The same
`rebuildstack.co/` prefix, the same `"false"` and only `"false"`, the same three
levels — Namespace, workload object, Pod — and on the workload object rather
than its pod template, for ADR 0028's reason: a template annotation is part of
the template hash, and opting out must not roll production.

The two controls nest rather than sit side by side. `collect: "false"` still
excludes profiling, because an excluded pod is not in the index any of this
reads; the profiling annotation is evaluated only for pods already admitted, so
it is strictly the narrower of the two. Both are read from the one owner-chain
walk `PodWatcher.admit` already performs, so the new control costs no lister, no
API read, and no second traversal.

A pod whose controller the agent cannot read — an Argo Rollout, a Knative
Revision, any operator's CRD — **fails open** and is counted by the same
`workload_unknown_kind` and `workload_not_cached` numbers ADR 0028 §4 defined. A
second pair of counters for the same blind spot would say the same thing twice,
and the reasoning is unchanged: an unreadable controller is not evidence that
anyone opted out, whichever annotation is being looked for.

**4. One decision, applied in three places, and those three are the whole of
both paths.** The decision is made once per pod, at admission, and recorded on
the pod's index entry. It is read by `targeting.Publisher`, which drops an
excluded workload before ranking; by `PodWatcher.ContainersOnNode`, which is the
only source of the container IDs a node's profiler acts on; and by
`PodWatcher.PodAddress`, which is the only place a pod address is read at all
(ADR 0057 §3) — so nothing can connect to an excluded pod.

The candidate funnel is filtered as well, and that one is not redundant. A
target confirmed before the annotation was written stays confirmed, since a
digest and a port do not change; left in the list it would be visited every
cycle, find no address, and hold one of the ten pull slots forever. Filtering
the funnel is also what makes one annotation stop the confirmation and the pull
together, rather than each of them separately.

The exclusion is per pod where the path can be, and per workload where it cannot.
One annotated replica of a Deployment contributes no containers and is never the
address asked; the other replicas are profiled as before. A workload is dropped
from a ranking or a funnel only when **every** indexed pod of it carries the
annotation — and a workload with no indexed pods is not dropped, because a scale
to zero must not be reported as something the customer asked for.

**5. Two numbers, two subjects, and the second one is a gauge.**
`collection_coverage.filter.excluded_profiling_annotation` counts pods that were
collected and not profiled. `collection_coverage.pprof.excluded_by_annotation`
counts the endpoint targets the funnel dropped. They are apart because they
count different things: one annotated Deployment of fifty replicas is fifty pods
and one target, and the questions "how much of my cluster declined profiling"
and "why does this service have no profile" are answered by different numbers.

Neither is broken out by which level carried the annotation. The collection
counters are, because there the customer needs to know which of their objects
removed a pod from the data; here the outcome is identical at all three levels
and the object is an identity this payload does not ship (CLAUDE.md invariant 6).

The target number is a gauge on `pprof_targets` and deliberately **not** an
outcome on `pprof_pulls_total`, which is where it was first asked for. There is
no attempt to count: the target leaves the funnel before anything is requested.
And the number *falls* when a customer removes an annotation, which a Prometheus
counter may not do — `TestCountersAndGaugesAreTypedByWhetherTheyCanFall` is the
test that would have caught it. The gauge sits beside `confirmed`, `absent` and
`unreachable`, which are states of a target in the same funnel, and it answers
the same question in the same payload.

**6. A capture is an event in the customer's cluster, so it is a line in the
customer's log.** Every pull attempt writes one line, at `Info`, on the message
`pprof profile pull`, naming the namespace, workload, container, image digest,
the seconds requested, and the outcome.

The coverage counters cannot do this job. They aggregate, they are cumulative
from process start, and they arrive at a backend that does not exist. Someone
investigating a ten-second CPU step at three in the morning needs the capture
that happened, on the workload it happened to, at the time it happened — not a
total since the pod started. One message string so it greps, one line per
attempt so it can be counted, and the outcome on the line so "why has this
service no profile" is answerable from the same grep.

The volume is bounded by the same construction that bounds the profiling: ten
attempts per five-minute cycle, so at most 2880 lines a day for a whole cluster.
Endpoint confirmation is not logged per event — it starts nothing, and a cluster
of several hundred images would emit a burst of them at start-up, which is the
shape that teaches an operator to filter a message out.

**7. The install says what it will do.** `NOTES.txt` gains a block, rendered
only where the thing it describes actually runs: what the agent connects to,
what a capture costs, the command that shows the captures, how to switch each
half off, and how to exclude one workload. A customer who learns this at
`helm install` has agreed to it — the same obligation ADR 0057 §4 discharged in
`security.md`, discharged again where an operator will actually be standing.

## Consequences

**Easier.** "Do not profile the payments service" is one annotation on the
object, with no rollout, no configuration change, and no loss of anything else
the agent knows about that service. The gap that made the four controls
all-or-nothing is closed at the only scope that mattered.

A customer running their own continuous profiler on one service no longer has to
rely on the six-hour hold of ADR 0058 §3 converging after a lost race; they can
say so in advance. And an operator can answer "did you profile us, and when"
from `kubectl logs` rather than from a payload nothing yet receives.

**Harder / given up.** A fifth control is a fifth thing to document, to test and
to keep true, and `rebuildstack.co/profile` is a name in the annotation
vocabulary that cannot now mean anything else. The coverage payload grows by two
numbers, in a payload whose whole discipline is that it grows slowly.

A namespace-level annotation takes effect on each pod's next event rather than
immediately, which is the property the collection annotation has had since ADR
0028 and is unchanged here — no namespace event re-evaluates pods.

The control does not reach a workload whose controller the agent cannot read,
exactly as ADR 0028's does not, and for the same refusal to ask for RBAC on
arbitrary custom resources. The namespace and pod annotations still work there.

**Not changed.** Pulling and discovery both still ship on (ADR 0058 §1). Which
workloads may be profiled is still which workloads are collected, with no
configured second list (ADR 0025 §1) — this narrows inside that scope by
annotation, which is the additive direction that ADR anticipated. No RBAC, no
new API read, no new capability, and no byte of any payload about a workload
that was not annotated.

One thing is better rather than worse, and it is worth naming beside ADR 0054's
inference channel. A collection opt-out makes a workload disappear from the
data, so which workload it was follows by subtraction. This one removes nothing:
the workload keeps shipping every payload it shipped before, minus a profile
that may never have existed. There is nothing to subtract.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
