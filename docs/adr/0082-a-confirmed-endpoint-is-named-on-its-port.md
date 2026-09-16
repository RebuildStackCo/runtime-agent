# 0082. A confirmed endpoint is named on the port it answered on

Date: 2026-09-16

Status: Accepted

Adds a `pprof` field to each port of `listening_ports`, carrying what the
prober's one request to that port answered.

## Context

Three facts about a workload's debug surface already ship, and the fourth — the
one that makes the other three a finding rather than a lead — does not.

| Fact | Payload | Identity |
|---|---|---|
| the binary links `net/http/pprof` | `go_build.pprof_endpoint` | image digest |
| the process listens on a port, loopback or not | `listening_ports` | workload, container, digest |
| six targets confirmed, eleven absent, two unreachable | `collection_coverage.pprof` | **none — counts only** |
| *which* port of *which* workload answered | — | **nowhere** |

The prober knows. It asks about a `Target{ImageDigest, Port}` and files the
answer under that pair (`internal/pprofprobe/probe.go`). What leaves the cluster
is `Snapshot()`, four integers.

The gap cannot be closed by joining what ships. Take the `listening_ports`
golden: `shop/web` links pprof, listens on 8080 and on 6060 bound to loopback.
Which of the two serves the mux? The payloads cannot say, and the difference is
the whole finding — a pprof mux on a routable port serves
`/debug/pprof/heap`, the contents of the process's memory, to anything that can
reach the pod, and `/debug/pprof/cmdline`, the argv that CLAUDE.md invariant 4
refuses to collect. On a loopback port it serves them to nothing.

An inference from "links pprof" and "has a routable port" is right most of the
time and wrong silently. A report that presents it as a finding spends the
credibility the report exists to build.

## Decision

**1. The answer is stamped onto the port, in the payload that already names
it.** `listening_ports` records carry `{namespace, workload, container, digest}`
and a list of ports; the prober's key is `{digest, port}`. The join is a lookup,
so no new payload, no new kind, and no new identity: the workload was already
named in this payload, on this record, one field to the left.

Stamping happens where the payload is assembled, and the prober does it —
`Prober.Stamp` — because the package that owns an answer should be the one that
writes it down. The store stays a pure join of what nodes reported.

**2. The value is a word, and its absence is not a fourth word.** `confirmed`,
`absent`, `unreachable` — the prober's own three states. A port nothing asked
about carries no field at all, and every loopback port is such a port:
`Candidates` drops them before the funnel, because nothing outside the pod can
open them.

A boolean would have collapsed "asked, and it is not pprof" into "not asked",
and the second is the common case. Proof of absence and absence of proof are
different claims and this payload makes both, so it must say which it is making.

**3. The controller's type is not the node's.** `inventory.Port` is new and
carries the node's observation plus the controller's answer;
`nodescan.ListeningPort` keeps carrying only what a node can know, which is what
crosses the node→controller channel. The alternative — one field on the node's
type that the node never fills — is one line shorter and blurs the line the
channel exists to draw (ADR 0009, ADR 0052's shape).

**4. Why this names a workload where the coverage payload does not.**
[ADR 0054](0054-coverage-says-how-much-was-hidden-never-what.md) reports counts
and no identities, and CLAUDE.md invariant 6 forbids the identity of a
*filtered-out* object from leaving the cluster. Neither is in the way here, and
saying why matters more than the field does.

The invariant protects the customer's refusal: they said do not look, and we do
not report on whom. This record is the opposite — an admitted workload, under
collection, and the fact is about that workload's own configuration, published
to that workload's own operator. A workload the profiling opt-out excluded is
never probed at all (ADR 0071), so it has no answer to stamp, and its silence
here is the same silence as everywhere else.

**5. The objection, named.** The payload becomes a list of debug endpoints
reachable inside the customer's cluster. Weak — the ports are already in this
payload, the mux answers any pod on the network whether or not we write it down,
and a reader of the spool is already inside — but weak is not absent, and an
unnamed objection is a hole in the argument (ADR 0039's discipline). It is
accepted because the alternative is not telling the operator that their memory
is readable.

## Consequences

**Easier.** The finding a report can make becomes provable instead of inferred:
this workload, this port, answered the pprof index. The loopback case, which is
the correct configuration and the common one, is legible as correct rather than
indistinguishable from the dangerous one.

The three coverage counters keep their meaning and are not replaced. They count
targets, including targets of workloads that have since gone; this field
describes ports that exist now.

**Harder / given up.** A second type where there was one, and a conversion in
`PortSnapshot` to go with it. The cost is real and it buys the channel's
boundary: a reviewer asking "can a node claim an endpoint is confirmed?" reads
the type and sees that it cannot.

The field describes the latest answer, not a history. A port that was confirmed
and is now unreachable reads as unreachable, and the payload does not say it
ever answered — the prober's own retry rule (`unreachableRetry`) decides how
long that takes to correct.

**Not changed.** No new request, no new read, no new connection: the answer
being published was already established by the probe ADR 0057 authorised, and
this ADR moves nothing but where it is written. `go_build.pprof_endpoint` still
answers the question about the image, and this field the question about the
address.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
