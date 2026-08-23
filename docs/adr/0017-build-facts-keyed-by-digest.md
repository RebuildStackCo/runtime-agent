# 0017. Immutable build facts ship once under the image digest; snapshots say when they were taken

Date: 2026-08-21
Status: Accepted
Amends: 0012 §1, §3
Amended by: 0018, 0019, 0027

Amends [ADR 0012](0012-payload-registry-and-provenance.md): §1's registry gains a
kind, and §3's "it is true as of the flush that wrote it" becomes a field
instead of a sentence. ADR 0012 is otherwise unchanged.

## Context

Three losses in the node→controller join, all of them silent.

**Dependency module paths were collected, transmitted, and thrown away.** The
node scanner has always extracted the dependency list from each kept binary's
build information, and `nodescan.BinaryInfo` has always carried it over the wire
(ADR 0010). The controller's join dropped it on the floor, because
`inventory.GoRecord` had no field for it. The cost was already being paid —
scan, serialize, transmit, parse — and only the last step was missing.
`docs/security.md` §8 already told customers those paths were kept, which made
the claim true of the node and false of everything downstream.

**The node name was dropped, and adding it to the record would have been
wrong.** `nodescan.Report` names the node that produced it; the join discarded
it. But a `GoRecord` merges every replica on every node running the same build,
so "node" is not a property of the record — putting it there is exactly the
scope error ADR 0014 exists to prevent. What was actually missing is not a field
on the record but the completeness of the record *set*: an inventory assembled
from facts that nodes push cannot distinguish "no Go workloads on that node"
from "that node's DaemonSet never started", and neither can anything downstream.

**No structural snapshot said when it was taken.** `go_inventory`,
`workload_metadata` and `node_metadata` each carry a `sequence`, which orders
supersedes, and nothing else. ADR 0012 §3 stated that a snapshot "is true as of
the flush that wrote it" — a fact present in the prose and absent from the
bytes. A consumer holding the newest snapshot could not tell whether it was
assembled a minute or a day ago.

The dependency data also poses a volume problem the other payloads do not. A
Go service commonly links 200–1500 modules. `go_inventory` is a superseding
snapshot rewritten on every flush; hanging module paths off each record would
have meant megabytes rewritten and retransmitted every minute, forever, for data
that never changes.

## Decision

**1. A build's dependency set is its own payload kind, keyed by image digest.**
`go_dependencies` carries one build's module paths, its Go version, and its main
module. It is not a field on `go_inventory`; the join back is
`go_inventory.records[].image_digest`.

The key is what makes this cheap. A digest identifies an immutable artifact, so
its dependency set is immutable too: written once, never superseded, never
recomputed when a second replica reports the same build. The registry of ADR
0012 §1 gains one row:

| Kind | Natural key | Delivery | Provenance |
|---|---|---|---|
| `go_dependencies` | image digest | accumulates, write-once | structural |

It is the first payload with neither a sequence nor a capture time, and
deliberately so: a sequence orders supersedes and this kind never supersedes; a
capture time dates an observation and this payload observes nothing that can
change. Its content is a property of the artifact, fixed when the image was
built.

That immutability is what makes the controller's bookkeeping safe. It remembers
in memory which digests it has already written and skips them, which is state —
but state whose loss costs one redundant write per build after a restart, and
whose redelivery is a byte-identical upsert. Loss-harmless in the sense of
ADR 0003, without an embedded database.

**2. Module paths, never versions.** The scanner already discards the version of
each dependency and this decision keeps it that way. A path says what a build is
made of; a path with a version is a vulnerability-scanning feed, a different
product with a different conversation about sensitivity attached to it. If that
is ever wanted, it is a new decision, not a field added quietly.

**3. `go_inventory` carries a `coverage` block.** Node count, facts received,
joined, unjoined, undigested — every counter cumulative from a `since` instant
carried alongside them, so the base is in the bytes rather than in this
document. This is ADR 0013's principle applied to a payload that is structural
rather than measured: the facts are declared, but *which* facts arrived depends
on a fleet of pushers, and that dependency has to be visible.

`nodes_reported` is the node name's real destination. Compared against the node
count in `node_metadata`, it turns "40 of 42 nodes have ever delivered
inventory" into an observable number, which is what makes a DaemonSet that never
started detectable from outside the cluster.

`facts_undigested` is new and counts a case that was previously invisible: a
fact the controller could join to a workload but whose container had no image
digest yet, so no build could be identified for its dependency set. Counting it
follows invariant 6 — the count leaves, the identity never does.

**4. `captured_at` on every structural snapshot.** `go_inventory`,
`workload_metadata` and `node_metadata` each carry the instant they were
assembled. The writers take it as an argument rather than reading the clock, so
the golden payloads stay deterministic.

It is a capture instant, not a window: a spec is not observed over time, so ADR
0012 §3's "no window" reasoning stands unchanged. And it dates the *assembly of
the snapshot*, not each fact in it — for `go_inventory` the node scans that
produced the facts finished at various earlier moments. How much of the fleet
those scans covered is what the `coverage` block answers; `captured_at` answers
only how old the snapshot is.

## Consequences

**Easier.** The two features this data was collected for become computable
outside the cluster: which builds already carry a `GOMEMLIMIT`-setting
dependency, and which carry modules that imply cgo. Neither list is frozen into
the agent — shipping the whole set and letting the analysis decide what is
interesting is what keeps a judgment call out of a fleet that updates on the
customer's schedule. A stale snapshot is now visibly stale, and an inventory
missing a node's worth of workloads is now visibly incomplete.

**Harder / given up.** Three golden payloads changed, which is a protocol change
for every consumer of them. A new payload kind is one more thing the backend
must ingest, and its write-once delivery is a discipline the backend must not
assume of the others. Dependency sets accumulate in controller memory with the
number of distinct builds observed since start — bounded by rollout frequency,
not by flush cadence, but not bounded absolutely.

Write-once also interacts with the spool's age sweep (ADR 0007) differently from
every other kind. A superseding payload that the sweep removes during a long
outage is rewritten by the next flush; a `go_dependencies` payload is not,
because the controller has already marked it written. An outage longer than the
spool's maximum age therefore loses that build's dependency set until the agent
restarts or the build is next observed fresh. This is the accepted shape of
ADR 0007 — durability is a knob, loss degrades to memory-only behavior — and the
loss is of an optimization input, never of a measurement.

**Not addressed here.** Build settings beyond the PGO flag (`CGO_ENABLED`,
`GOARCH`, `-trimpath`, `vcs.*`) and node architecture are the natural next step
for the same features and are deliberately left out of this decision. So is a
lifetime rule for `go_inventory` records: a record for a workload that no longer
exists is currently kept until the controller restarts, which the `coverage`
block makes more visible but does not fix.

This ADR records a decision implemented in the same pull request, per the
process in [`README.md`](README.md).
