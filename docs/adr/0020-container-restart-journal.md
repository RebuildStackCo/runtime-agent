# 0020. The restart journal: windowed aggregates, exact counts, sampled reasons

Date: 2026-08-22
Status: Accepted
Amends: 0012 §1
Amended by: 0024

Amends [ADR 0012](0012-payload-registry-and-provenance.md): the payload registry
gains `container_restarts`. ADR 0012 requires a new kind to be added by
extending that table rather than by inventing a `kind` string at a call site;
this is that extension, and nothing else in ADR 0012 changes.

| Kind | Natural key | Delivery | Provenance |
|---|---|---|---|
| `container_restarts` | (window start, window length) | supersedes within its window | journal |

## Context

The agent has had a journal since `oom_kill`, and exactly one thing in it. The
code that reads terminations discards everything else in a single line:

```go
if term == nil || term.Reason != oomKilledReason { continue }
```

So a container killed by the kernel is reported, and a container that has exited
1 forty times in an hour is invisible. The mechanism for reading, deduplicating
and shipping the second was already written; only the filter stood in the way.

This matters for the numbers the agent already sends. A container that keeps
dying uses little CPU and little memory, and from the outside its usage rollup
looks like a workload that is comfortably over-provisioned. The restart count is
what distinguishes "does not need those resources" from "never lives long enough
to use them".

Two properties of the source data shape everything below.

**The count is exact; the reason is not.** `restartCount` is a counter, so its
delta counts every restart, including ones the agent never saw individually. But
a container status carries only its *most recent* termination, so when several
restarts land between two informer updates, exactly one reason is visible.

**A restart has no timestamp.** Kubernetes records when the last termination
finished, and nothing about the ones before it. A counter that reads 40 the
first time the agent sees a container says nothing about when those 40 happened.

## Decision

**1. Restarts are reported as a windowed aggregate, not as one payload per
restart.** The record is per (namespace, pod, container) per hour-aligned
window, and the payload is one file per window holding many records — the same
shape as a usage snapshot.

The alternative, one payload per event as `oom_kill` does, would hand the
spool's file count to a crash loop. The spool is bounded by age alone
(`DefaultMaxAge = 24h`, swept by time, with no count or byte cap), and a
container in CrashLoopBackOff restarts roughly twelve times an hour: a hundred
such containers write ~1200 files an hour and ~28000 before the first is swept.
The latent problem has always been there; removing the OOM-only filter is what
would have made it live.

It is also the better fit for the question. An individual OOM is meaningful on
its own — it carries the memory limit that was in force. An individual restart
is not; "47 restarts this hour, 40 of them exit code 1" is the fact worth having,
and that is a property of the window.

**A cap on events per pod was considered and rejected.** It would have kept the
per-event shape at the cost of silently dropping data, and a payload that looks
complete while being truncated is the failure mode ADR 0018 exists to end.

**2. The key names the pod.** Every other payload counts replicas rather than
listing them (ADR 0012 §5), and `workload_metadata` says so explicitly. The
journal is the exception, deliberately: which replica is in a crash loop cannot
be recovered from a per-workload total, and it is the first thing anyone asks.
`oom_kill` has always named the pod; this makes the practice a documented
decision and enumerates it in `docs/security.md` §8, where it was previously
absent. The customer's control is unchanged and already enforced — namespace
filters, label selectors, and the opt-out annotation, which since ADR 0018
removes what was already collected as well.

**3. `restarts`, `reasons` and `reasons_unobserved`, and the arithmetic between
them.** The exact total and the sampled breakdown are separate fields, and their
difference is a third field rather than an absence:

```
sum(reasons) + reasons_unobserved == restarts
```

Folding them into one would have meant either under-reporting restarts (report
only what a reason was seen for) or inventing reasons for restarts nobody
observed. This is ADR 0013's principle — the agent reports what it saw and how
much it saw — applied to a journal payload rather than a measured one.

**4. Unfamiliar reasons are bucketed, not passed through.** Reason strings come
from the kubelet and the CRI, not from user input, so they are not a leak risk.
They are a cardinality risk: the set belongs to implementations we do not
control, and a map whose keys a container runtime chooses is a payload whose
shape it chooses. Known reasons keep their names; anything else is counted under
`other`. Counted, not dropped — the restart still appears in the total and in a
visible bucket.

**5. A container's first observation is baselined, never reported.** A counter
already standing at 40 describes restarts at moments Kubernetes does not record.
Reporting them would place a history in whichever window happens to be open when
the agent starts. This is the one place the journal is knowingly incomplete, and
it is bounded: it costs the restarts that predate the agent's view of a pod,
once per pod.

**6. Reasons yes, messages no.** `lastState.terminated.reason` is drawn from the
kubelet's own vocabulary. The adjacent `message` is free text and routinely
carries node names, taints and other workloads' details. Same reasoning, and the
same answer, as the build-settings allow-list of ADR 0019: the structured field
is collected, the free-form one is not.

**7. Windows align with the usage windows.** The journal takes its window length
from `collector.UsageWindowLength` rather than declaring its own, so a restart
count is always read against the CPU and memory of the same hour. Two constants
that merely happened to match would eventually not.

## Consequences

**Easier.** A usage rollup that looks over-provisioned can be checked against the
restart record for the same window and the same container. The overlap with
`oom_kill` is a feature: the window says how often, the events say how bad.
Memory and spool cost are bounded by the number of containers that restarted,
not by how often they did.

**Harder / given up.** The individual timestamps of restarts are not reported.
Nothing is lost that Kubernetes records — only the last termination is
timestamped — but a consumer wanting "the exact second of restart #12" will not
find it. A closed window is written once and then dropped from memory, so a
failed write loses that window; this is the bound the usage accumulator already
accepts (ADR 0007), and it applies here for the same reason.

`reasons_unobserved` will be non-zero on exactly the workloads people care most
about, because a fast crash loop is what outruns the informer. That is honest
rather than convenient, and the backend contract in
[`backend-requirements.md`](../backend-requirements.md) requires the breakdown
never to be rendered as if it were the total.

**Known gap, recorded rather than hidden.** `oom_kill`'s filename embeds the
namespace, pod and container names unescaped. Kubernetes names cannot contain
path separators, so this is not a traversal risk, but their combined length can
exceed the 255-byte filename limit for a long pod name. `container_restarts` is
unaffected — its filename carries only the window — and the OOM path is
untouched by this ADR.

**Not addressed here.** Pod-level lifecycle events (unschedulable, evicted) and
object-level history (ReplicaSet revisions, Job timings) are the rest of the
journal and are deliberately left to their own slices. Unschedulable in
particular is partly visible already, as the replica-count shortfall of ADR 0012
§5, and adding it means adding a *reason* to a signal that exists, not a new
signal.

This ADR records a decision implemented in the same pull request, per the
process in [`README.md`](README.md).
