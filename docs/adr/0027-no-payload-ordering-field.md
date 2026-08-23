# 0027. No payload carries an ordering field: the spool holds one version of a key

Date: 2026-08-23
Status: Accepted
Amends: 0006, 0012 §3, 0017, 0018, 0026

Removes the `sequence` field from every payload that carried it and the five
counters that produced it. Amends ADR 0006's requirement that the backend order
supersedes by an agent-side sequence, ADR 0012 §3's statement that a metadata
snapshot is "ordered by its sequence", ADR 0017's description of the structural
payloads, and ADR 0018's account of how the backend accumulates history. Closes
the open item ADR 0026 recorded under *Not addressed here*, by a third route
neither of the two it named.

## Context

Five counters produced the field, one per flusher, each a plain variable in the
process:

```
internal/collector/usage.go   p.sequence++     usage_snapshot
cmd/agent/main.go             goInventorySeq++ go_inventory
cmd/agent/main.go             metadataSeq++    workload_metadata, node_metadata
cmd/agent/main.go             restartSeq++     container_restarts
cmd/agent/main.go             disruptionSeq++  pod_disruptions
```

None of them is recoverable, and none may be: persistent state must be
rebuildable from the cluster or the backend (CLAUDE.md invariant 5), and a
counter is rebuildable from neither. So every controller start emits `1` again,
against a contract that reads:

> Ordering MUST follow the agent-side snapshot sequence — never arrival order.

For the three window-keyed kinds the damage is small: the key contains the
window, so a minute later it changes and the low sequence lands unopposed. For
the three keyed by the payload kind itself — `go_inventory`,
`workload_metadata`, `node_metadata` — the key never changes. A controller that
has run for a week reaches roughly ten thousand; after a restart it counts from
one, and a backend obeying the contract must reject its metadata until it climbs
back. The cluster's structural picture freezes for as long as the controller had
previously been up.

The obvious repairs are all worse than the field. Persisting the counter is
forbidden outright. Deriving it from the clock makes it identical, digit for
digit, to the `captured_at` these payloads already carry — two fields, one
value, no answer to which is authoritative. An epoch beside the counter adds a
compound comparison to the backend and still rests on the same clock.

Which raised the question that settled it: **what would order these payloads if
the field did not exist?**

**The spool cannot offer two versions of a key.** `Spool.write` marshals into
`<key>.tmp` and renames it over `<key>`. There is one `workload-metadata.json`.
Version 6 replaces version 5 on disk before anything ships, so the agent never
holds both.

**A resend cannot be stale.** Retransmitting an unacknowledged payload re-reads
the file, which by then holds the same version or a newer one. A duplicate
delivery of a superseding payload is idempotent.

**Only concurrency could invert them** — two requests for one key in flight at
once, the older winning the race. That is a property of a shipper that has not
been written, and "one request per key at a time" is free to guarantee.

**And the protocol already lives without ordering where it matters most.**
`usage_window`, the final record of a closed window and the most authoritative
measured payload the agent produces, has never carried a sequence. Its
supersession rule is by kind: closed replaces snapshot. If ordering were
load-bearing, that is the last place it would have been omitted — and nobody has
ever read it as a hole.

The last argument for keeping a counter is gap detection, and the contract
forecloses it: spool expiry and outages make gaps routine, and the backend "MUST
NOT treat a gap as an error". There is nothing to count.

## Decision

**1. No payload carries an ordering field.** `sequence` is removed from
`usage_snapshot`, `go_inventory`, `workload_metadata`, `node_metadata`,
`container_restarts` and `pod_disruptions`, and the five counters are deleted.
Superseding ingest is last-write-wins under the natural key.

**2. The obligation moves to the agent, where it can be met without state.** The
shipper keeps at most one request per natural key in flight. This replaces a
backend MUST that the agent could not honor across a restart with an agent MUST
it can honor by construction, and it is recorded in
`backend-requirements.md` §4 before the shipper exists.

**3. `captured_at` stays what it is: a fact, not machinery.** The three
structural payloads keep it because ADR 0017 added it to answer "how old is this
snapshot", which is still a real question. It is not repurposed as an ordering
key, and it is not to be nudged forward to keep a series monotonic — that would
corrupt an observation to serve a metadata concern. If a future need for a total
order appears, it gets its own field and its own decision.

**4. The window-keyed kinds gain nothing.** Adding `captured_at` to
`usage_snapshot`, `container_restarts` and `pod_disruptions` would be defensible
on its own merits — a snapshot taken five seconds into a window means something
different from one taken fifty — but that is a different decision with a
different justification, and it is not smuggled in under this one.

## Consequences

**Easier.** Six payloads lose a field, five counters and their comments leave
the agent, and the one behaviour they were meant to produce — the newest write
wins — is now produced by the spool's own shape rather than asserted alongside
it. A restarted controller is no longer a special case for anybody.

The tests that used the counter to prove supersession now prove it with
something a consumer can actually act on: the surviving metadata file is the one
with the later `captured_at`, the surviving restart window is the one with the
higher restart count, the surviving usage snapshot is the one with more polls
attempted. Each of those was the fact the test meant to check all along; the
counter was standing in for it.

**Harder / given up.** A backend cannot detect that it applied two versions of a
key out of order, because nothing distinguishes them but content. The exposure
is one flush cadence of staleness — every superseding kind is rewritten in full
each minute — and it requires the shipper to violate point 2 first. Weighed
against a documented freeze lasting as long as the previous uptime, this is the
cheaper failure by a wide margin, and unlike that one it heals itself.

There is no longer a way to ask "how many flushes has this agent done", which
nothing asked.

**Not changed.** What is collected, filtered or shipped. Six goldens lose one
line each and nothing else moves — `usage-window`, `go-build`, `oom-kill` and
`profile` are byte-identical, which is itself the evidence that the field was
absent from the payloads that needed ordering least and most.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
