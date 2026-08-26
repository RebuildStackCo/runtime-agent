# 0043. Both halves of the counter reading come from one observation

Date: 2026-08-26

Status: Accepted

Amends: 0034 §7

Takes `restarts` from the agent's own record of the counter rather than from the
informer's store, so the reading and the history it excludes are always the same
observation. No change to the payload's shape, its fields, or any golden file.

## Context

`restart_counters` reports two numbers a consumer is told to subtract: the
counter's value, and what it already stood at when this agent process first saw
the container. ADR 0034 §2 says their difference is what the windows should
contain, and §3 says stating it beats leaving it to subtraction.

The two came from different places. `restarts` was read from the informer's
shared store through the pod lister; `restarts_before_observation` from the
`restartCounts` map the agent maintains in its own event handler. client-go
updates the shared store **before** it dispatches to that handler, so for the
span of one dispatch the store holds a counter the baseline has not seen.

Usually that is invisible — the counter only goes up, and the reading is
slightly stale in a way nobody could detect. It becomes visible when the counter
goes *down*, which is a StatefulSet replacing a pod under the same name: the
store already says 2 while the baseline still says 40. The payload then asserts
that forty restarts predate an observation of a counter standing at two, and the
subtraction ADR 0034 prescribes yields **−38**.

This was showing up as a test that failed about one run in three under package
load, which is the shape a race takes when nobody has looked at it yet:

    restart_counters_test.go:180: restarts_before_observation = 40, want 2
        — the old pod's 40 are not this pod's history

The payload is a superseding snapshot, so the next flush corrects it. That
bounds the damage and does not excuse it: while it stands the payload is wrong,
and it is wrong in the direction that inflates history the agent never watched —
the exact quantity ADR 0034 exists to report honestly.

## Decision

**1. Both counter values come from one read of the baseline.** `restarts` is
`base.last` — the counter as the agent last observed it — and
`restarts_before_observation` is `base.atFirstSight` from the same struct, taken
under one lock.

This makes the ordering structural rather than probable. A rebaseline sets
`last` and `atFirstSight` together; an advance only ever raises `last`. So
`restarts >= restarts_before_observation` holds by construction and their
difference cannot be negative, whatever the informer is doing at the time.

**2. `restarts` means "as the agent last observed it", and says so.** This
amends ADR 0034 §7, which describes the reading as assembled from the live pod
index at an instant. The rest of the record still is — pod creation time,
container start, last termination. The counter is not, and the difference is
one informer dispatch against a payload that flushes once a minute.

Naming the lag is better than hiding it: everything else in this payload is
already a statement about what this agent process observed, which is the whole
point of ADR 0034. A counter read from somewhere the agent has not yet processed
was the odd one out.

**3. The interleaving is a test, not a timing hope.** The regression test builds
the state directly — baseline at 40, store at 2, handler not yet run — because
that is what the informer produces for a moment on every counter change. Waiting
for it to happen by chance is what let this sit unnoticed.

## Consequences

**Easier.** The two numbers a consumer subtracts can no longer disagree. A
backend that trusted ADR 0034 §2 arithmetic will not receive a negative
difference, and does not need a defensive clamp for a case the agent should
never have produced.

**Harder / given up.** The counter can lag the cluster by one informer dispatch.
For a payload assembled once a minute this is not a cost anyone can measure, and
the alternative — reading the fresher number — is what produced the defect.

A related inconsistency stays and is smaller: `last_termination` still comes
from the pod, so it can describe a death the counter has not yet counted. That
direction is benign — it under-reports the count rather than inventing history —
and it self-corrects on the next flush. Recorded so it is a known shape rather
than a surprise.

**Not changed.** No field is added or removed, no golden file moves, no RBAC, no
collection. Only which of two equal-in-the-common-case numbers is written.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
