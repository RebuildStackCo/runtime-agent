# 0089. A window says what changed

Date: 2026-09-19

Status: Accepted

Amends: 0054, 0088

Adds the payload kind `agent_counters` and a seventh journal accumulator,
resumed at startup on ADR 0077's terms. No new collection, no new privilege, no
new read: its input is the counters `collection_coverage` already carries, taken
from the same pass.

## Context

ADR 0088 gave the agent's two states a window and named what it left: the
counters have the same blind spot for the same reason, and subtracting a
previous reading is a different mechanism. This is that mechanism.

Every number in `collection_coverage` is cumulative from `since`. That answers
how much and never when:

```
filter.excluded_namespace_annotation: 12
```

A namespace annotated an hour ago and one annotated three weeks ago produce the
same twelve. The customer's data stops arriving for a workload and the counter
that explains it cannot say whether the explanation is today's.

Differencing successive payloads on the other side does not rescue it. The kind
supersedes — the contract says upsert, last write wins — so keeping a history is
something the backend was never asked for; a restart moves `since` and starts
the counters over, so any interval spanning one is unrecoverable; and the
resolution would be whatever the backend happened to retain.

Where a subtraction belongs was settled once already. ADR 0062 moved the
`/proc` counter subtraction onto the node because the node holds both readings
and the key that makes them comparable, and set the rule that matters more than
the arithmetic: three cases produce **no delta rather than a wrong one**. The
same shape applies here, with `since` as the key instead of a process start
time.

## Decision

**1. A window kind for the agent's own counters.** `agent_counters`, one record
per `(window, counter)`, hour-aligned, superseding within its window, written
every pass — the shape `agent_states` already has, so it resumes through the
same machinery and the backend implements one thing twice rather than two
things.

**2. The counter's name is its path where it is reported cumulatively.**
`filter.excluded_pod_annotation`, `pprof_pull.refused`,
`intake_rejections.malformed`. The name is the join, so this payload says when
and never restates what: the meaning of each number stays in the one place that
already documents it, which is the rule ADR 0024 set for prose and holds for
schemas.

**3. What is counted, and what each exclusion was excluded for.** The filter's
pod and Job counters, the puller's four outcomes, the receiver's three
rejections, and the kubelet poll attempts and failures. Left out:

- the placement and node drops — they describe the shape of the data, not
  behaviour over time;
- the endpoint and goroutine coverages — they count targets by their latest
  answer, which is a snapshot and not a total, and subtracting two snapshots
  produces a number with no referent;
- the inventory, scan and eBPF blocks — fleet aggregates over per-node
  counters, which nothing may subtract across a changing set of nodes;
- the shipping counters — the one class whose reader can already date them. The
  backend issued those statuses itself and saw the gaps in `captured_at`
  itself, so a window here would tell it what it wrote down.

**4. The subtraction is keyed by `since`, and refuses rather than guesses.** A
pass whose base differs from the previous pass's accrues nothing: the readings
belong to different processes and are not subtractable. A counter that fell
under an unchanged base accrues nothing either — whatever produced it, the
reading is not of the series the last one described. Both are ADR 0062 §1, with
`since` in the role of the process start time.

**5. The interval splits at a window boundary; the rise does not.** Observation
is a duration and is attributed to both windows it spans. A rise is not: it
happened at an instant no pass can see, so it lands whole in the window the pass
observed it in. Splitting it by elapsed time would be inventing the instant, and
the error is bounded by one pass, which is one minute.

**6. Observation travels with every counter, including one that did not move.**
A record with a zero delta says the agent was running and nothing happened; no
record at all says nobody was counting. That distinction is the reason a window
with nothing in it is still written, and it is ADR 0054's founding one.

## What this still cannot say

- **A restart loses the rises inside its own gap.** The new process's counters
  start at zero, so there is no reading anywhere that spans the outage: what
  happened while the agent was down is counted by nobody. `observed_nanos` is
  what makes the hole visible rather than silent.
- **A rise within one pass of a window boundary may land in the later window.**
  Bounded by the pass interval and not correctable without an instant the agent
  does not have.
- **A delta is not a count of objects.** The counters count decisions, not pods
  (ADR 0054), and a window of them inherits that exactly.

## Consequences

**Easier.** "This workload's data stopped on Tuesday — why?" gets an answer in
the same hour as the question: the exclusion counter that explains it moved in
that window or it did not. The same for profiles that stopped arriving and for
a node whose reports began being refused.

**Harder / given up.** An eighth kind in the startup read and a second payload
per hour whose subject is the agent. Both are records of a few dozen bytes; what
they cost is a reader's attention, which is why the counter list is closed and
argued above rather than "every number we have".

**Not changed.** What is collected about the cluster, what is filtered, what is
read, any other kind's cadence or key, or the one-way protocol. Nothing here
names an object: a counter name is a counter name, and its value is a number of
decisions.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
