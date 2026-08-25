# 0034. The restart counter is shipped as a reading, so the day-one history is not lost

Date: 2026-08-25

Status: Accepted
Amends: 0020 §5

Adds the payload kind `restart_counters`. Nothing existing changes shape.

Amends [ADR 0020](0020-container-restart-journal.md) §5: a container's first
observation is still baselined and never reported as an advance — the windows
are unchanged — but the baselined number is no longer discarded. It ships here,
in a shape that can carry it.

## Context

ADR 0020 §5 decided that a counter already standing at 40 is not reported,
because those restarts happened at instants Kubernetes does not record and
placing them in whichever window is open at startup would invent a history. The
reasoning was right and remains right. What it did not ask is whether the
*number* has anywhere else to go, and the answer was assumed to be no.

The cost lands precisely where the agent is supposed to be strongest. The whole
point of the object journal (stage 3 of the implementation plan) is a report on
the day of installation, containing numbers about the past rather than only
about the present. `job_runs` and `deployment_revisions` deliver that: a Job
carries its own timestamps, and ReplicaSets carry theirs. The restart journal
does not. On install day it reports zero restarts for a cluster whose pods have
restarted thousands of times, and it will keep reporting zero for as long as it
takes the crash loop to recur — which for a workload that fails once a week is a
week.

The same hole opens every time the controller restarts. The spool is an
emptyDir (ADR 0026) and the accumulators live in memory, so a rescheduled
controller starts with an empty journal and re-baselines every container it
sees. That is not an install-day quirk; it is every deploy, every node drain,
every eviction of the agent's own pod.

And the counter is not hard to get. It is in `status.containerStatuses[].
restartCount`, in the pod status the agent has watched since its first slice.
It was being read and thrown away.

## Decision

**1. The counter ships as a reading, not as events.** One record per collected
container that has ever restarted, carrying `restarts` — the counter's value at
this capture — and shipped as a superseding snapshot under a fixed key, the
shape the structural snapshots use.

This is what makes it honest. A journal record is a claim that something
happened during a window; the agent cannot make that claim about restarts it did
not see. A counter reading is a claim about an instant — "at 10:12 this counter
read 42" — and that claim is true regardless of when the restarts behind it
happened. The shape carries exactly what the source supports, which is why the
number that could not be reported as a window can be reported as a reading.

**2. `restarts` and the journal windows are never added.** The reading is the
total; the windows are a subset of it. Their sum can only be smaller, and the
difference is what the agent did not watch.

**3. How much the agent watched is a field, not an inference.**
`restarts_before_observation` is what the counter stood at when this agent
process first saw the container, and `observed_since` is when that was. The
difference between the reading and that number is what the windows should
contain; the number itself is what no window will ever contain.

Stating it rather than leaving it to subtraction matters because subtraction
would require the consumer to have every window since the agent started, and to
know that none of them was lost. It also degrades correctly: after a controller
restart the field simply reports the larger number the new process found, which
is the truth about *that* process's view.

**4. The interval is reported, not a rate.** `pod_created_at` and
`observed_since` bound the stretch of time
`restarts_before_observation` is spread over, and the agent says nothing about
how they are distributed inside it. A rate would be a conclusion, and the agent
draws none (ADR 0004); an interval is a fact about the source's own limits.

`container_started_at` is the third instant, and the one that turns a total into
a situation: forty restarts under a container that has been up for twelve days
is a healed workload, and forty under one that has been up for thirty seconds is
an incident. Both read `restarts: 40`.

**5. A container with a zero counter produces no record, and absence is the
claim.** The payload is a snapshot, so what is not in it is stated to be zero —
the same logic ADR 0032 §2 applied to unconstrained workloads. The alternative,
a record per container in the cluster, would say the same thing at fifty times
the size.

The payload is written even when no container has ever restarted, with an empty
record list. This is where a snapshot differs from a journal: the journals write
nothing in a quiet hour, because an empty window says only that the agent was
running, while an empty reading says something about the cluster.

**6. The termination reason is carried as written.** ADR 0020 §4 buckets
unfamiliar reasons under `other`, because there the reasons are *keys* of a map
and a payload whose key set a container runtime chooses is a payload with
unbounded shape. Here the reason is a value of a fixed field, where an unfamiliar
string costs nothing structural — the same treatment `unscheduled` reasons and
an autoscaler's `limited_reason` already receive. The free-text `message` beside
it is still never read (ADR 0020 §6).

**7. It rides the metadata flush and its capture instant.** The reading is
assembled from the live pod index at an instant, exactly as workload metadata
is, and it carries the same `captured_at` so a counter can be laid beside the
replica counts and the shape it belongs to. A second capture time would let a
consumer pair a reading with a workload from a different moment.

**8. Scope runs through the admitted pod index.** A pod excluded by a namespace
filter or an opt-out annotation is not in the index, so it is unreachable here by
construction rather than by a second check — the property ADR 0030 established
and ADR 0032 reused. The record names the pod, for the reason ADR 0020 §2 gives:
which replica is restarting is the question, and no per-workload total can
answer it.

**9. No new RBAC.** The counter is in the pod status the agent already watches.
Nothing is read that was not being read; what changes is that it is no longer
discarded.

## Consequences

**Easier.** The report on install day contains the restart history of the
cluster, which is the plan's own definition of done for the journal and was not
met. A controller restart stops erasing what the cluster still knows. And the
reading gives the backend a completeness check the journal could not provide on
its own: the gap between the total and the windows is measurable rather than
assumed to be zero.

**Harder / given up.** Two payloads now describe restarts, and a consumer that
adds them double-counts. The contract in
[`backend-requirements.md`](../backend-requirements.md) says so, and §2 above is
the rule, but it is a shape that can be misused in a way a single payload could
not.

The pre-observation restarts remain undatable. This ADR does not recover their
instants — nothing can, short of reading Events, which default to a one-hour TTL
and would cost a new grant to add nothing beyond that hour. It moves them from
"not reported" to "reported as a total over a stated interval", which is the
best the source supports.

A pod that dies before a flush takes its unwatched history with it: the snapshot
describes live pods, so a record disappears when its pod does (ADR 0018). What
the agent watched while the pod lived stays in the windows.

**Not changed.** The restart journal's shape, windows, arithmetic and bucketing
are untouched, as is its rule that a first observation is never reported as an
advance. No other payload changes, and no permission does.

This ADR records a decision implemented in the same pull request, per the
process in [`README.md`](README.md).
