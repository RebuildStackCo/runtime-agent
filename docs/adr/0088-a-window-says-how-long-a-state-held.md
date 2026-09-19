# 0088. A window says how long a state held

Date: 2026-09-18

Status: Accepted

Amends: 0054, 0087

Adds the payload kind `agent_states` and a sixth journal accumulator, resumed at
startup on ADR 0077's terms. No new collection, no new privilege, no new read:
its input is the two states `collection_coverage` already carries, taken from
the same call on the same pass.

## Context

ADR 0087 gave `sources[].failing` and `shipping.halted` the instant they began,
and said plainly what it did not do: a state that ended is absent from the next
payload by design. That is not an oversight in the field — it is what a state
field is. It describes the present, and this kind supersedes, so the backend is
entitled to keep one row.

So the defect ADR 0087 closed was half of one. "Is it broken, and since when" is
answerable. "Was it broken" is not:

```
13:47  failing_since: 13:47     the payload says so
14:06  (no field)               recovered — and the row is clean
                                nineteen minutes are now unrecoverable
```

Two different questions hide under one word. A state that **holds** is a fact
about now, and a boolean with a start is the right shape for it. A state that
**held** is a fact about an interval, and no field describing the present can
carry it.

This repository has already answered a question of exactly this shape.
`container_restarts` is a window and `restart_counters` is a reading, and the
contract carries the sentence that keeps them apart: *"`restart_counters` is a
reading, and MUST NOT be added to the windows"* (ADR 0034). One fact in two
shapes, with the reader told which is which. The six window kinds beside it
share one key — `(window start, window length)`, hour-aligned — and ADR 0078
made an open window declare itself open. The agent's own behaviour was the last
subject still reported only in the pre-window shape.

What is deliberately **not** in scope is the counters. `filter.excluded_*`,
`shipping.deferred`, the puller's outcomes and the intake rejections are
cumulative from `since` and have the same blind spot for the same reason, but
subtracting a previous reading is a different mechanism with its own failure
modes (ADR 0062 priced them), and it is not decided here.

## Decision

**1. A window kind for the agent's own states.** `agent_states`, one record per
`(window, state, subject)`, hour-aligned, superseding within its window, written
every pass as a slice of the open window on ADR 0078's terms. Subjects are the
resource classes the agent watches and the agent's own shipping; no object in
the customer's cluster is nameable here, at any resolution.

**2. Occupancy and episodes, not events.** Each record carries how many
nanoseconds of the window the subject spent in the state and how many episodes
began in it. Two numbers rather than a record per transition, because the cost
is then bounded by construction at whatever flap rate — and because one
nineteen-minute outage and three six-minute ones are different diagnoses that a
single duration would merge.

**3. Accrual moves forward, and never re-measures.** A pass attributes the
interval since the previous pass, split at any window boundary it crosses.
`accrued_to` is how far a record is true, and is where a later process resumes
from; `open_since` is the marker that tells that process the episode it finds
open is the one already counted rather than a new one. Occupancy is never
recomputed as `now - open_since` — that expression is correct exactly once and
double-counts every span before a restart, which is the whole reason the marker
exists rather than a start instant alone.

**4. Sampling at the pass loses nothing here, and that is measured.** A pass
runs every minute (`everyPass`), and a run of watch failures becomes a new
episode only after two minutes of quiet (`watchStreakGap`). Two episodes
therefore cannot both begin and end between two passes, and a single one that
does is still seen, because the state is defined with a three-minute tail. The
halt cannot flap at all: it latches until the process ends.

**5. Observation travels beside occupancy.** Zero occupancy against a full
window is a healthy hour; zero against zero observation is an hour nobody
watched. Without the second number they are the same bytes, which is ADR 0054's
founding defect arriving in a new shape. A restart's gap is in neither: the
first pass of a process accrues nothing, so time the agent was not running is
never claimed as time it was watching.

**6. Every watched subject writes a record, including a clear one.** A subject
with no record is one nobody watched. This is why a window with nothing wrong in
it is still written, where the journals beside this one write nothing.

**7. An episode is counted where it was seen, not where it began.** Back-dating
an entry to the instant the episode started would reopen a window that had
already ended and been written final. The instant is not lost — it is in
`open_since` and in the occupancy split across the boundary.

## What this still cannot say

Stated here rather than discovered later:

- **A restart across a window boundary adds one entry.** The earlier window has
  ended and is not reopened (ADR 0077 §1), so the continuing episode is seen as
  new. It is detectable: `since` in `collection_coverage` moves at the same
  moment, so an entry adjacent to a moved `since` is suspect.
- **An episode that closed between two passes undercounts by up to one pass.**
  The instant it closed is not knowable from two readings, and the alternative
  is to invent it.
- **The marker survives a process restart, not a pod replacement.** The spool is
  an `emptyDir` by decision (ADR 0007, ADR 0026); a rescheduled agent resumes
  nothing, as ADR 0077 §4 already priced.

## Consequences

**Easier.** An outage that ended is reportable for the first time. "No finding
from EndpointSlices this week" acquires an answer that is neither a guess nor a
trip into the customer's logs, and `observed_nanos` makes every finding that
rests on a window quotable with the coverage it actually had.

**Harder / given up.** A seventh kind in the startup read, so `security.md`'s
closed list moves again — the mechanism ADR 0077 declared and ADR 0080 then
failed to use. One file per hour of a handful of records: negligible in bytes,
and one more payload the backend implements.

**Not changed.** What is collected about the cluster, what is filtered, what is
read, any other kind's cadence or key, or the one-way protocol. The two states
this measures are the two ADR 0087 named, read from the same call that writes
them to `collection_coverage`, so the window and the snapshot cannot disagree.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
