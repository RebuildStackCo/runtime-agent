# 0016. Prometheus is not a data source — not as an option, not for backfill

Date: 2026-08-20
Status: Accepted

Supersedes the framing of Prometheus in [ADR 0006](0006-usage-collection-kubelet-rollups.md)
§Context, which rejected it for the usage path while leaving it standing as "a
side channel present in most clusters". ADR 0006 is otherwise unchanged: its
kubelet sources, merge property, and window contract stand.

## Context

The security document advertised a Prometheus integration the agent has never
had: a row in the network-access table (`query` / `query_range`, "historical
metrics over the retention window; enables a report on installation day"), a
mention in the `metrics-only` profile, and §10.1 "Prometheus is a side channel",
which disclosed its tenancy weakness as an accepted compromise.

Nothing in the code reads Prometheus. So the document promised a capability that
did not exist — the mirror image of the §10.2 defect closed in ADR 0015, where
the code fell short of the promise. Both are the same failure: a customer-facing
claim not pinned to a decision.

The pull to build it is real, and it always arrives phrased as temporary — a
day-one report needs history, the agent has none until it has run, and the
customer's Prometheus already holds months of it.

**The reason to refuse is not that Prometheus data is low quality. It is that
its quality is unmeasurable.** A `query_range` response is a series of numbers
with no provenance attached. Recording rules, downsampling, and
`metric_relabel_configs` all shape it, and none of them leaves a trace in the
response. The agent cannot tell whether a series is raw, five-minute-averaged, or
dropped and re-derived; it cannot tell whether a workload is missing because it
did not exist or because a relabel rule discarded it. Every safeguard this agent
has built exists to prevent exactly that: ADR 0012 makes each payload declare its
provenance, ADR 0013 makes a measured payload declare how much of the window it
observed and whether each signal was seen at all. A Prometheus series can satisfy
none of those, so it would have to enter as a class whose completeness is
permanently unknown — and be joined, under the same natural keys, with data whose
completeness is exact.

There is also a tenancy problem, which the old §10.1 already admitted: a
Prometheus endpoint typically serves all namespaces regardless of the caller's
Kubernetes permissions, so the customer's namespace filters would become a soft,
agent-side boundary on that path — while every other path in this agent enforces
them at collection (ADR 0015).

## Decision

**Prometheus is not a source of agent data. Not as a default, not as an opt-in,
not as a one-time backfill.** No configuration key, no Helm value, no profile
enables it. The agent's measurements come from the kubelet counters of ADR 0006
and nothing else.

The prohibition is on Prometheus as a *source of data about the customer's
workloads*. It does not touch:

- **the `github.com/prometheus/*` Go modules in `go.mod`**, which parse the
  kubelet's own `/metrics/cadvisor` text exposition. That is the kubelet's
  format, not a Prometheus server, and the counters are ours to read directly.
- **the customer running Prometheus.** They usually do; the agent simply does not
  read it. Its own resource use may of course be scraped by it.
- **exposing agent metrics** in the exposition format, if that is ever wanted.
  That is data flowing outward, and it carries no claim about a workload.

**What we give up, stated plainly: the agent knows nothing about the time before
it was installed.** A first-day report is therefore built from what is true at
install time and needs no history — declared requests and limits, QoS,
topology and placement, probe configuration, the object journal (conditions,
restart counts, ReplicaSet revisions, Job timings) — not from backfilled
measurements. Everything that genuinely requires observation says how long it has
been observing (`covered_nanoseconds`, ADR 0013) and is worth exactly that much.

## Consequences

**Easier.** Every measured number in the protocol has a known provenance and a
known completeness, with no exceptions to carve out downstream. Namespace filters
stay enforced at collection on every path. One fewer external dependency, one
fewer endpoint in the network table, one fewer credential.

**Harder / given up.** No historical backfill, ever. A cluster installed today
has no usage rollups for yesterday, and a report that depends on a long window
must wait for that window. This is the cost, it is accepted deliberately, and
"just this once, to seed the account" is the exact request this ADR exists to
refuse — recording it now, while nothing is built, is the whole point.

**Cost of the decision itself: zero code.** No Prometheus client was ever
written. This ADR removes three claims from `docs/security.md` and adds a
sentence a future reader can point at. It is being recorded at the cheapest
moment it will ever be available.

This ADR records a decision implemented in the same pull request — here, as
documentation and a prohibition rather than code — per the process in
[`README.md`](README.md).
