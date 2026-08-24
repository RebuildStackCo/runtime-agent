# 0033. A permission the agent was not given degrades one payload, and says so

Date: 2026-08-24

Status: Accepted
Amends: 0032 §1

Changes how the agent behaves when it cannot read a policy source, and adds an
`unavailable_sources` line to `workload_policy` and `cluster_policy`. No new
kind, no new RBAC, no change to what is read when the read succeeds.

## Context

ADR 0032 added seven resources to the ClusterRole and put their informer caches
in the list the agent waits on before it starts. That was a reflex rather than a
decision, and it produced a failure mode out of all proportion to its cause: a
customer who removed one line from the ClusterRole — say `storageclasses`,
because they did not see why a cost agent needs it — got an agent that failed to
start and collected nothing at all, usage included.

The alternative of quietly skipping what cannot be read is worse than it looks,
because of what ADR 0032 §2 decided: a workload that nothing constrains produces
no record. In a superseding snapshot that absence is a positive statement. If
budgets are unreadable, the statement becomes false — and not only at field
level. A workload whose only constraint was a budget disappears from the payload
entirely rather than appearing with a field missing.

So silence has to be qualified, or it lies twice.

## Decision

**1. An informer that gates a signal blocks; an informer that adds one does
not.** The owner-chain and namespace caches stay in the blocking list: a pod
admitted before its controller was cached would be admitted without its
workload-level opt-out being checked, which is a correctness property. The seven
policy caches gate nothing, so nothing waits on them.

This is the rule, not a per-resource judgement, and it is what should have been
applied in ADR 0032.

**2. Readiness is read at snapshot time, and the signal is `HasSynced`, not "the
list came back empty".** That choice is the whole mechanism. A cluster with no
PodDisruptionBudgets lists zero objects and syncs normally; a cluster that denied
the permission never syncs at all. An empty result and a refused read are
therefore different states rather than the same silence — which is precisely what
a consumer needs and cannot reconstruct afterwards.

It also needs no timeout and no error classification. The cause may be RBAC, a
webhook, or an unreachable API server; "not available at this capture" is true in
every case, and the agent does not have to guess which.

And it heals: the reflector retries indefinitely, so a permission granted later
appears at the next flush without restarting the agent.

**3. Each payload declares its own sources**, and the list is at payload level
rather than inside a record — it is a property of the capture, not of a workload.

`workload_policy` declares budgets, autoscalers and claims; `cluster_policy`
declares limit ranges, quotas, priority classes and storage classes. They are not
pooled, because the two have distinct natural keys and are upserted
independently: one can arrive without the other, so each must be readable alone.

The storage-class catalog is deliberately absent from the workload list. A
claim's `storage_class` is a name read from the PersistentVolumeClaim, not from
the catalog; the catalog says what that name *means*, which is a cluster-policy
question.

Names are resource classes, never customer objects, so nothing here can identify
a workload (CLAUDE.md invariant 6). The field is omitted when empty, and empty is
itself the statement: every source was read, so what is not in the payload does
not exist in the cluster.

**4. The field is about this capture and no other.** A cache still filling when
the first flush fires is reported unavailable, truthfully, and is simply present
at the next one. It is not a claim that the source is permanently gone, and a
consumer must not read it as one.

**5. Known limitation: a permission revoked while the agent runs is not
detected.** `HasSynced` is a one-way latch, so a cache that filled once and then
lost its watch keeps reporting availability while serving its last contents. The
data is stale rather than false, and any restart reports correctly.

Detecting this needs a watch-error handler that can tell `Forbidden` from an
expired resource version. That is deliberately not built: the event is rare, the
damage is bounded to staleness, and the complexity is not. It is recorded here so
it stays a decision rather than becoming an oversight.

## Consequences

**Easier.** The blast radius of an RBAC mistake is now the payload it concerns.
A customer can decline any of the seven grants — deliberately or by accident —
and keep the usage collection they installed the agent for, while the report
shows exactly what was not readable rather than quietly reading as complete.

The declared line also answers a question `security.md` could not answer before:
what happens if you do not grant this.

**Harder / given up.** Two payloads can now be internally complete and still be
missing data, which a consumer must handle rather than assume away. That is the
honest shape of the situation, and the alternative was to encode the assumption
in silence.

**Not changed.** Nothing about what is read when the read succeeds. No new RBAC,
no new kind, no change to any other payload's shape, and the blocking behaviour
of the caches that gate pod admission is untouched.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
