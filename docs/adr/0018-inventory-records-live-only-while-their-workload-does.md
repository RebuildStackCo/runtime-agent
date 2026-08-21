# 0018. The Go inventory forgets: a record lives only while its workload does

Date: 2026-08-21
Status: Accepted

Amends [ADR 0017](0017-build-facts-keyed-by-digest.md): its `coverage` block
gains a lifetime rule for `nodes_reported`, and the record-lifetime question it
recorded as "not addressed here" is answered. ADR 0017 is otherwise unchanged.

## Context

`inventory.Store` was the one place in the agent whose keyspace only ever grew.
`Ingest` had exactly one operation on it — assign — and no code path anywhere
removed a record. A record entered when a node reported a binary that joined,
and left only when the controller restarted.

This was not an oversight so much as a missing half. Every other payload is
derived from state the controller can re-read: `workload_metadata` is rebuilt
from the live informer index on every flush, so it drops what the cluster
dropped without anything having to remember that it should. The inventory
cannot be rebuilt that way, because it is a join of facts the *nodes push* with
state the controller holds, and node reports arrive only once per scan
interval. Accumulating was the right answer to that. Eviction was the part that
was never written.

Three consequences, in ascending order of seriousness.

A deleted workload's record kept shipping in every snapshot, so the backend saw
a Go service that no longer existed. Worse, `workload_metadata` had already
forgotten it, so the two payloads disagreed about which workloads exist —
detectable by joining them, but only by a consumer who thought to look.

**The serious one: an opt-out that did not apply to what had already been
collected.** When a customer annotates a pod with `rebuildstack.co/collect:
"false"`, `PodWatcher` drops it from the index immediately, and the controller
stops naming it in the scan scope it hands to nodes (ADR 0015). Both halves
worked. What followed was that *no new fact ever arrived* about that pod — and
a store that only accumulates has no way to act on the absence of facts. The
record already held kept leaving the cluster, with its namespace, workload name,
container name and main module path, in every snapshot, for as long as the
controller ran. `docs/security.md` §11 said "the exclusion applies at the
collection stage". For this payload it applied only to facts not yet collected.

The mechanism to fix it was already in place and had one consumer too few:
`indexPod`'s own comment says the index "drops a pod the moment" it is excluded,
and the metadata snapshot was built on exactly that guarantee.

## Decision

**On every flush, the inventory forgets whatever the controller's own filtered
pod index no longer supports.** A record whose (namespace, workload kind,
workload name, container) is absent from the live index is dropped before the
payload is written; the dependency set of any build no surviving record
references is dropped with it.

The live index is `PodWatcher.Pods()` — the same source `workload_metadata` is
derived from. That is the substance of the decision, not an implementation
detail: the two payloads now agree about which workloads exist *by
construction*, because they read one index, rather than by two code paths
happening to stay in step.

Three properties follow, each of which was a decision:

**No empty-input guard.** `Retain` with an empty live set empties the
inventory. A guard would have been the intuitive safety measure and would have
been wrong: a cluster where nothing passes the filters legitimately has an empty
inventory, and a guard would pin the last non-empty snapshot forever — the exact
failure this ADR exists to end. It is safe without one because the index is only
ever mutated by informer events and never wiped: `PodWatcher` registers its
handlers after the caches sync, and a relist replays as adds and updates. An
empty index means an empty cluster, not an unsynced one.

**`deps` and `written` are dropped together.** Dropping a dependency set while
leaving its digest marked as written would mean that if the image ever came
back, its dependency set would be re-ingested and never sent — silently, with
nothing failing and no test naturally covering it. This is the failure mode the
two-step `PendingDependencies`/`MarkDependenciesWritten` split of ADR 0017
exists to make visible, and it deserves the same care here.

**`nodes_reported` gets the same treatment.** ADR 0017 described it as
cumulative since a `since` instant. That was wrong in the same way, and this
corrects it: a node that has left the cluster is dropped from the count. Its
whole purpose is comparison against the node count in `node_metadata`, and a
count that outlives its nodes exceeds the fleet — so a scaled-down cluster with
a broken DaemonSet would have looked complete, which is precisely the case the
block was added to expose.

## Consequences

**Easier.** The opt-out promise is now true of every payload, and provable: the
end-to-end test annotates a running pod and asserts its record leaves the
payload. Memory is bounded by what currently runs rather than by everything ever
observed. `go_inventory` and `workload_metadata` can be joined without orphans.

**Harder / given up.** A record's absence now carries meaning it did not carry
before — "this workload is not currently collected", not "we have not learned
about it yet" — and the backend must not confuse the two. The `coverage` block
is what distinguishes them.

A short-lived workload flickers. A CronJob whose pods live thirty seconds
appears in the snapshot covering that minute and is gone from the next. This is
accepted, and it is the same reasoning that kept the "interesting modules" list
out of the agent in ADR 0017: `go_inventory` is a current-state snapshot, and
history is accumulated by the backend across the sequence of snapshots, which
carry `sequence` and `captured_at` for exactly that. "Workloads that have ever
been Go" is a question the analysis answers, not the agent.

Such a workload also re-sends its dependency payload each time it returns, since
the written mark goes with the set. One payload per reappearance, byte-identical
under a key whose ingest is an upsert: churn, not error.

Finally, a pod that a node scanned before the controller's informer saw it loses
its record for at most one flush cycle and regains it on the next scan. The
alternative — trusting a fact about a pod the controller cannot confirm exists —
is the one this agent consistently refuses (ADR 0015).

This ADR records a decision implemented in the same pull request, per the
process in [`README.md`](README.md).
