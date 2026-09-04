# 0067. A record carries the instant its source last stated it

Date: 2026-09-04

Status: Accepted

Amends: 0052, 0056, 0062

The node-fed payloads gain `asserted_at`, `go_inventory.coverage` gains the list
of reporting nodes with the instant of each one's latest report, and
`collection_coverage` gains what the receiver refused. No new collection, no new
privilege, no change to what the node sends: the controller already knew when
each report arrived, and why it turned one away, and was throwing both numbers
away.

## Context

The question that found this was narrow: a node whose report exceeds the
receiver's 4 MiB bound is rejected whole, and nothing on the controller logs or
counts it — would anyone notice? The answer split in two, and the second half is
what this ADR is about.

**A node that never reports is visible.** It never enters `nodes_reported`, and
that count is deliberately comparable with the node count in `node_metadata`
(ADR 0018). A shortfall shows.

**A node that reported and then stopped is not.** It stays in `nodes_reported`
— the set is added to on ingest and pruned only when the node leaves the cluster
— so the count stays whole. Its contributions stay in the store, and every flush
folds them into `process_peaks`, `process_counters` and `listening_ports` under
a `captured_at` that is always current, because the controller is alive. At
14:00 the backend receives a peak measured at 10:00, stamped 14:00, with nothing
to tell the two apart. That is the more likely trajectory, because a report
crosses a size bound by growing.

The store's own comments state the mechanism precisely, three times:

> a node's contribution is **replaced wholesale on its next report**, so a peak
> survives only while a process on some node still holds it (ADR 0052 §3)

Decay is driven by **arrival**, not by time. The only event that can age or drop
a fact is the arrival of its replacement. If the replacement never comes there is
no decay at all — and "the node stopped saying this" has nowhere to be recorded.
Absence is not merely undetected here; in that structure it is unrepresentable,
which is why every remedy considered from outside it (a rejection counter, a
liveness probe, a per-node timer bolted onto the coverage report) needed a
denominator and a threshold invented for the occasion.

This failure has a precedent in this repository, described in almost the same
words. ADR 0035:

> `HasSynced` is a one-way latch. It turns true when the first LIST succeeds and
> never turns back. A cache whose watch is refused afterwards therefore keeps
> reporting availability while the store answers every query from what it last
> held.

`nodesReported` is that latch for the node channel. The defect was found,
reasoned about and closed for informers; the node channel reproduces it because
the resemblance is hidden by the mechanism — the store is ours, filled by a push,
and looks nothing like a watch.

## Decision

Every node-fed record carries the instant its source last stated it.

The store already keys each contribution by node, and a report replaces that
node's contributions wholesale — so all of one node's live contributions were
stated in the same pass, and one instant per node is the exact assertion time of
each of them rather than an approximation of it. `nodesReported` therefore
becomes `map[string]time.Time` and is the whole of the new state.

- On a record merged across nodes (`process_peaks`, `process_counters`,
  `listening_ports`), `asserted_at` is the **oldest** contributing node's latest
  report: a merged record is only as current as its stalest contributor.
- On a `go_inventory` record, which is last-writer-wins across nodes, it is that
  writer's instant.
- `collection_coverage.scan.oldest_asserted_at` bounds that block's sums the
  same way.
- `go_inventory.coverage.nodes` names each reporting node and its instant. It is
  the only place a node is named — records are keyed by workload — and it is
  what answers *which* node went quiet.
- `collection_coverage.intake_rejections` counts what the receiver refused on
  its two push paths, by reason: unauthorized, too large, malformed. An instant
  that stops advancing says a node went quiet; this says whether the controller
  is the one turning it away, which no other signal distinguishes — a rejected
  report appears otherwise only in that node's own log, in the customer's
  cluster, where nobody looks until something prompts them. The query paths are
  not counted: a refused query costs a node its next pass, not data it holds.

The instant is read from the controller's clock, the same one that stamps
`captured_at`, so the two are comparable without a skew term. Nothing is added
to the wire from the node.

**No threshold, and no expiry.** The agent does not decide what "too old" is and
does not drop a stale contribution. Two reasons. The controller does not know a
node's scan cadence — it is a flag on the DaemonSet, absent from the controller's
schema — so any threshold it picked would be blind. And a verdict is a judgement,
which ADR 0004 keeps out of the agent: ship the numbers the answer is made of.

Alternatives considered:

- **A liveness endpoint on the node, polled by the controller.** It measures the
  wrong thing: a node whose report is rejected for size is a healthy process
  whose data does not land, and the probe is green in exactly the failure that
  prompted this. It also costs an inbound socket on the most privileged pod in
  the install (`hostPID`, `SYS_PTRACE`, and under the ebpf profile `CAP_BPF` and
  `CAP_PERFMON`), authentication in a direction that does not exist today, and
  the reversal of "the node initiates every connection" (ADR 0010).
- **A capture instant in the report.** It cannot help: in the failure case there
  is no report to read it from. It would also import clock skew between nodes and
  the controller, which measuring on arrival avoids entirely.
- **Expiring a quiet node's contribution.** ADR 0035 settled the shape of this
  answer: a gating source stops the agent, everything else degrades its payload.
  The inventory gates nothing, so it declares. Expiry can be added later, on
  evidence, and would make a workload's peak fall between flushes — which today
  it never does.
- **Per-fact timestamps.** Correct and redundant: wholesale replacement makes
  them all equal to the node's, so storing them per fact would be the same
  number written many times.

## Consequences

A consumer can tell a measurement restated a moment ago from one an hour old,
which it could not before, and `captured_at` stops being mistaken for the age of
what it stamps. `backend-requirements.md` now states that as an obligation.

The four payloads grow by one instant per record, `go_inventory` by one entry per
reporting node — the same per-node cost `node_metadata` already pays — and
`collection_coverage` by three counters.

Whether and why are answered together. A node whose `asserted_at` has stopped
advancing while `intake_rejections.too_large` climbs is a node outgrowing the
channel; one with no rejections at all is quiet for a reason the controller never
saw — crashed, unscheduled, or refused by a NetworkPolicy — and that difference
is what an operator needs before they can look anywhere.

The 4 MiB receiver bound is unchanged. It was only ever a problem because the
loss was silent, and silence is what this removes.
