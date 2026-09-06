# 0070. The agent says what it did, in the format the cluster already reads

Date: 2026-09-06

Status: Accepted

Amends: 0013 §3, 0069 §1

Adds a `/metrics` path to the health listener of both roles, exposing the
counters the agent already keeps. No new collection, no new privilege, no new
port, no payload byte changed, and no counter that did not already exist except
the spool's own.

## Context

The agent measures a cluster and says nothing about itself. What it does say
goes to two places, and neither reaches the person who is on call for it.

**A structured log line.** It travels the customer's log pipeline, where an
alert on it means writing a query against a text field, per counter, per
cluster. Nobody does that for a vendor's agent.

**`collection_coverage`.** It carries the same numbers to the backend, which is
the right audience for "what does this report rest on" and the wrong one for
"page me when collection stops" — and today it reaches nobody at all, because
the transport is not built (ADR 0054, `security.md` §8 Transport **[planned]**).
So the one artifact that exists to distinguish an empty cluster from a blind
agent is itself written to a spool nothing reads.

The result is that the customer's SRE has no way to alert on the failure this
repository has spent six decisions making *observable*: a watch that stopped
being fed (ADR 0035), a kubelet path that fails while the other keeps working
(ADR 0013), a node that reported and then went quiet (ADR 0067), a fleet whose
profiler never loaded (ADR 0060), a spool that is evicting payloads faster than
they can be shipped (ADR 0042). Every one of those is computed, in memory, right
now.

And the port question was settled a slice ago. ADR 0069 §1 put the probes on
9090 rather than beside 8080 with the reasoning stated in advance: *"9090 rather
than a number beside 8080, because the metrics endpoint of a later slice joins
this listener as a path. One port means one container port, one probe target and
one hole in the network policy, now and after metrics land."* This is that
slice, and it changes no chart template.

## Decision

**1. The exposition is `prometheus/common/expfmt`, and no new dependency.**

The obvious choice is `client_golang`, and it is the wrong one twice over.

It is a new module in a build gated on reachable vulnerabilities (ADR 0038):
every future finding in it, and in the `procfs` and `expfmt` code it pulls
behind it, becomes a red build for a package whose entire job is to render
numbers this agent already holds. Writing the text format by hand is worse — a
standard format reimplemented for no reason, which is code with no reason.

There is a third option, and it is the one taken: `github.com/prometheus/common/expfmt`
and `github.com/prometheus/client_model` are **already direct dependencies**.
`internal/collector/cadvisor.go` *decodes* this exact format with
`expfmt.NewDecoder` to read the kubelet. Encoding with `expfmt.NewEncoder` adds
no module to the build, gives the reachability gate nothing new to gate, and the
format is still the reference implementation's rather than this repository's.

The second reason is the one that decided it. `client_golang`'s shape is a
process-wide registry of counter objects that call sites increment. Adopting it
invites `metrics.PodsExcluded.Inc()` beside the filter's own counter, and then
there are two answers to "how many pods were excluded" that drift silently —
which is precisely what §2 exists to prevent. `expfmt` has no registry to
increment: a metric family can only be **built from a snapshot at scrape time**.
The rule stops being a convention a reviewer must remember and becomes the only
thing the API permits.

**2. One set of counters, read by two audiences.**

Every number on the endpoint comes from the same accessor `collection_coverage`
is assembled from — `Filter.Snapshot`, `PodWatcher.SourceHealths`,
`Store.Counters`, `Prober.Snapshot`, `Puller.Snapshot`, the intake rejections,
the heartbeat that `/livez` reads. There is no second counter anywhere in this
change, and the one place where the payload and the metrics wanted different
shapes was resolved by making the *source* finer and the payload a sum of it,
never by adding a number beside one.

That place is the kubelet paths. ADR 0013 §3 states that `/stats/summary` and
`/metrics/cadvisor` are separate requests because they fail independently, and
then counts them together, so the payload cannot say which one is failing. The
poller now counts per path and `Observation()` returns the sums: the payload's
bytes are unchanged, and the endpoint gains the distinction the ADR always
claimed. This is the pattern for every future divergence — refine the source,
derive the coarse view — and never the reverse.

The endpoint is the SRE's, the payload is the backend's, and they cannot
disagree, because there is nothing for them to disagree about.

**3. Label names come from a closed set of nine; no value names anything in the
cluster.**

`role`, `version`, `source`, `path`, `reason`, `outcome`, `kind`, `state`,
`subject`. `TestOnlyTheseLabelNamesAreAllowed` pins the list, the builder
refuses a name outside it, and a second test walks the *actual* exposition of
both roles — not the builder's guard — so a metric added with a tenth label
fails whether or not its author read this.

This is a cardinality budget and the continuation of invariant 6 at the same
time, and the second is why it is a rule rather than advice. A namespace label
would put the identity of a collected object on a port that any pod in the
cluster can reach (§5 below), through a channel no payload review covers. The
counts here are the same aggregates `collection_coverage` carries, and they name
nothing for the same reason it names nothing.

One value does not originate here: a node's profiling state crosses the channel
from the DaemonSet. It is mapped through the enumeration in ADR 0060 §2 with
everything else bucketed to `other`, so a node running a newer build cannot open
a series in the controller's exposition.

`kind` is not enumerated in this package at all. Its values are
`sink.Registry()`, and the spool's per-kind counters are seeded from the same
list, so every registered kind has a series at zero before it has ever shipped —
which is the point, because *"no `usage_window` has been written in an hour"* is
the alert, and an absent series cannot express it. `Spool.write` now takes the
kind, refuses one the registry does not hold, and every caller passes the same
string it puts in the payload's bytes. The registry stays the one list (ADR
0022), and it is now load-bearing at run time rather than only in the goldens.

**4. Both roles, and the node names itself nowhere.**

ADR 0054 §4 decided that the coverage payload is aggregate over the fleet and
never per node, so "how many processes did the fleet skip" is answerable and
"which node stopped" is not — ADR 0067 then closed half of that with
`asserted_at`, which says a node went quiet without saying which one.

A per-node scrape closes the other half without touching a payload. The node
identity comes from the customer's own service discovery, which already knows
which pod it scraped; the agent contributes no label for it. What the node
exposes is its own latest pass, its own profiler's state, and whether its
reports are reaching the controller — the last of which was invisible on the
node until now, since a delivery failure only ever reached its own log.

`pass_start_timestamp_seconds` and `pass_deadline_seconds` come from the same
`Heartbeat` `/livez` reads, so an alert written on them fires on exactly the
condition the kubelet restarts the pod for — and, unlike the probe, before the
restart erases the evidence.

**5. No chart change, and the exposure that follows from that.**

The path joins the listener ADR 0069 already opened: same container port, same
probe target, same NetworkPolicy rule, no `ServiceMonitor` and no Prometheus
Operator CRD in the chart — the chart installs an agent, not somebody else's
monitoring stack, and a CRD it does not own is a dependency on a component half
of its installations do not run.

The consequence is stated rather than left to be found (ADR 0039): ADR 0069 §5's
rule admits the health port from **any** source, because the caller is the
kubelet and no `podSelector` can name it. So any pod in the cluster can scrape
these numbers. That is the second reason §3 is a rule: what such a pod learns is
how many pods this agent observed and how many its filters excluded, in
aggregate, and nothing about which. Discovery is left to the operator's own
scrape configuration rather than to `prometheus.io/*` annotations, which the
major operator-based stacks do not honour and which would turn a default-on
annotation into a promise this chart cannot keep.

**6. The spool's own numbers are new, and stop at the endpoint for now.**

Four counters did not exist: payloads written per kind, writes that failed,
evictions by which bound caused them, and the size and file count the last sweep
measured. ADR 0042 gave the spool hard ceilings and no way to see them act — a
cluster whose payloads are being evicted oldest-first looks exactly like one
whose payloads are being kept, which is the failure that ADR most needed to be
visible.

They are exposed and **not** added to `collection_coverage` in this slice. That
would be a protocol change, and it belongs with the transport that gives it a
reader. Because they are single-source by construction, adding the block later
reads the same snapshot and cannot diverge from what the endpoint serves.

## Consequences

**Easier.** "The agent stopped collecting" becomes an alert an SRE writes in one
line against a timestamp, with the threshold supplied by the agent rather than
guessed. Five failures this repository already knew how to detect and could only
write to a log become alertable: a watch no longer fed, one kubelet path failing
while the other works, a node that reported and went quiet, a fleet whose
profiler never loaded, a spool evicting under its ceiling. The endpoint works on
an installation with no backend at all, which is every installation today.

**Harder / given up.** A second consumer now depends on the shape of these
counters. Renaming a metric is a breaking change for whoever alerts on it, in a
way that renaming an internal field never was — the counters were free to move
while only a log line and an unshipped payload read them, and they are not any
more. That is the cost of being usable.

The exposition is built per scrape, from locks the flush pass also holds. A
scrape is a handful of map reads and cannot block collection meaningfully, but
it is not free, and a scrape interval below the collection cadence buys nothing:
most of these numbers change once a pass.

Any pod on the cluster network can read the agent's aggregate counters, as §5
sets out. The alternative was a source-restricted rule that cannot be written in
a template that does not know the cluster's node CIDR — the same wall ADR 0069
§5 hit, with the same answer.

**Not changed.** No payload byte, no golden file, no RBAC rule, no capability,
no mount, no chart template, no collection. `internal/sink/registry.go` gains no
row; it gains a second reader. `docs/security.md` §5 gains one row because a
port answers one more question, not because the agent learns anything new.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
