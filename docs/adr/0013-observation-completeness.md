# 0013. Observation completeness: measured payloads carry what the agent saw

Date: 2026-08-20
Status: Accepted
Amends: 0006 §3, 0012
Amended by: 0022

## Context

[ADR 0012](0012-payload-registry-and-provenance.md) fixed the provenance classes
and recorded the gap it left open: the measured payloads did not yet say
anything about their own collection. Three consequences of that gap were
already visible in the code.

**A zero was ambiguous.** `ThrottledPeriods` and `TotalPeriods` were plain
counters with a zero default. A container that was never throttled and a
container whose node's `/metrics/cadvisor` scrape failed for the whole window
produced byte-identical records. The same held for both PSI stall counters. The
cluster-wide signal set — which existed, but only in a log line — could say "this
cluster exposes throttling" and still not resolve the question for any single
record, because a scrape fails per node and per path, not per cluster.

**A partial window looked like a quiet one.** A record's `Samples` counted rate
observations, but nothing said how much of the window those observations spanned.
A container that started fifty minutes into an hour and one whose node was
unreachable for fifty minutes both produced a small number with no way to tell
which.

**Only the agent can know any of this.** From outside the cluster a failed scrape
is unrecoverable after the fact — there is no later query that reveals it. This
is a fact about collection, not a judgment about the data, so it belongs in the
payload rather than in a log the backend never sees.

Separately, the throttling counters were keyed differently from every other
kubelet counter, and that was a defect rather than a decision — see below.

## Decision

**1. Per-signal sample counts on the record.** `CPU.ThrottlingSamples`,
`CPU.PSISamples` and `Memory.PSISamples` count the observations that actually
carried each signal. Zero means never observed; non-zero makes the accompanying
zero total mean "observed, and it was quiet". The counts are attributed to the
window holding the interval's last instant — the same rule rate samples already
follow — and add under merge, so the merge property is unchanged.

**2. Per-record coverage.** `CPU.CoveredNanoseconds` is the summed duration of
the counter intervals attributed to a record, split across windows exactly as
the counters themselves are. Against `WindowSeconds` it answers "how much of this
window did we see?" without the agent deciding what a sufficient answer is.

**3. Cluster-wide collection state on the payload.** Measured payloads carry an
`observation` block: the poll cadence, cumulative counts of kubelet requests
attempted and failed, and the signal set this cluster exposes. Both kubelet
paths (`/stats/summary` and `/metrics/cadvisor`) count as separate requests
because they fail independently — and which of them failed is resolved per record
by the sample counts of (1), not here. The counters are cumulative since agent
start, like every other counter this agent reports: the backend takes
differences between deliveries, and no window-scoped bookkeeping is invented for
them.

**4. Provenance completes.** `usage_snapshot` and `usage_window` declare
`source: measured`, `oom_kill` declares `journal`, `go_inventory` declares
`structural`. The registry in ADR 0012 §1 is now fully realized in the bytes; the
gap that ADR recorded is closed.

**5. Throttling keys on the pod UID.** The cAdvisor exposition labels containers
by pod *name*, and the poller kept its counter baselines under that name while
CPU and memory kept theirs under the pod UID. A name is not an identity: a
StatefulSet recreates a pod under the same name with a new UID, and the
replacement's counters were then compared against a dead container's baseline —
dropped as a phantom counter reset, or, when the new counters happened to exceed
the old baseline, stretched into one continuous run. The exposition's name is now
resolved to a UID before any state is kept, so all three kubelet signals share
one identity.

## Consequences

**Easier.** A backend can state what a report rests on and refuse to conclude
from a record it did not see enough of. "Throttling is not a problem here" is
now distinguishable from "we could not observe throttling here", which is the
difference between an answer and a guess. Coverage also carries the
container-lifetime case for free: a container that ran for ten minutes of the
window covers ten minutes of it, without the agent modelling lifetimes.

**Harder / given up.** The golden bytes of every usage, OOM and inventory payload
changed — one migration, taken deliberately and all at once rather than field by
field later. Records grew four `int64`s. The observation block is cluster-wide,
so it cannot attribute a failure to a node: it says three scrapes failed, not
which node lost them. Per-node attribution would mean node-scoped payloads that
nothing yet needs; the per-record sample counts already answer the question that
matters for any given record.

**Deliberately not done.** No "expected sample count" is reported. It would
require the agent to model when a container was supposed to exist, which is a
judgment; `CoveredNanoseconds` against the window length is the same information
without the modelling. No reason string for non-coverage: the agent reports the
counters, and the backend renders the cause.

This ADR records decisions already implemented in the same pull request, per the
process in [`README.md`](README.md).
