# 0026. The controller needs no persistent volume, and therefore no StatefulSet

Date: 2026-08-23
Status: Accepted
Amends: 0007 §1, 0008 §3

Removes the `persistence.enabled` opt-in of ADR 0007 §1 and the write-free
strict mode ADR 0008 §3 built on top of it, and settles the workload kind ADR
0008's Lifecycle section assumed. Closes the fifth and last of the open items
ADR 0022 §5 recorded. ADR 0007's loss analysis and ADR 0008's decision to keep
identity in a namespaced Secret are unchanged — what goes is their alternative,
not their choice.

## Context

ADR 0007 made disk a knob: `emptyDir` by default, `persistence.enabled: true`
swaps in a PersistentVolume for installations that want unacknowledged data to
survive rescheduling. ADR 0008 then built a second product on that knob —
"strict mode", where identity lives on the volume instead of a Secret and the
chart emits no write grant at all.

Neither exists. `persistence.enabled` appears in five places and none of them is
code:

```
docs/adr/0007 §1, §Consequences   the decision
docs/adr/0008 §3                  strict mode
docs/security.md §4, §Storage     the promise to the customer
docs/backend-requirements.md §4   the contract
internal/config/config.go         a comment above the spool struct
```

That is not by itself an argument against it — the chart does not exist either.
What settles it is what building it would cost against what it would buy.

**What the volume buys the spool is small.** `emptyDir` already survives
container restarts on the same node, the most common restart class. The volume
changes the outcome only where a backend outage and a pod reschedule coincide,
and even there nothing irreplaceable is lost: the superseding kinds are rewritten
by the next flush, `go_build` is re-pushed by the node on its own cadence, and
`oom_kill` is keyed by its content so a redelivery is the same row. The one thing
genuinely gone is a window of `ebpf_profile` — a sampled signal, where a missing
window is a missing sample and not a wrong answer. `MaxAgeHours` caps the spool
at a day regardless, and the coverage report makes any gap visible rather than
silent.

**What strict mode would cost is two identity implementations.** One writing key
and certificate to a Secret through the API, one writing them to a file with its
own permissions, corruption and half-write story, both carried through the
enrollment flow of ADR 0005 and both tested. It doubles the slice that has not
started.

**And the customer it serves is hypothetical.** The grant strict mode avoids is
already the narrowest Kubernetes permits: a namespaced Role, `resourceNames`
pinned to one object, `get` and `update`, no `create`, `list`, `watch` or
`delete` (ADR 0008 §2). In most enterprises that is a shorter review than a
PersistentVolume, which needs a StorageClass and provisioning rights. We have no
customer who refuses it, and the last configuration field this repository wrote
for a customer nobody had was the profiling eligible set — which shipped dead in
our own manifest and cost ADR 0025 to remove.

**The workload kind was waiting on the same question.** ADR 0008's Lifecycle
section states "the controller is a single-replica StatefulSet: one writer, no
races"; `deploy/controller.yaml` has always shipped a `Deployment`. ADR 0022 §5
recorded the discrepancy as harmless today and load-bearing by the time identity
ships.

Examined, the StatefulSet earns less than it appears to. `replicas: 1` in a
Deployment does not mean at-most-one — the default rolling update rounds
`maxSurge` up to one and `maxUnavailable` down to zero, so every upgrade runs two
controllers for as long as the new one takes to become ready. A StatefulSet
forbids that. It also forbids the replacement from starting when a node goes
NotReady, until the old pod is confirmed gone: at-most-one bought with unbounded
downtime.

For this controller downtime is the more expensive failure. ADR 0015 and ADR 0025
make every node fail closed without a scope, so a controller stuck Terminating on
a partitioned node stops scanning and profiling across the whole cluster until
someone forces the delete. Lost telemetry is bounded and visible; a stalled
collector is neither.

And the race the StatefulSet was invoked against is better closed where it
happens. `update` on the identity Secret takes a `resourceVersion` precondition,
so a second writer loses with a conflict, re-reads, and finds the identity that
won. That is a guarantee. A StatefulSet is a weaker one, holding only until
somebody runs `kubectl delete pod --force` or edits the replica count.

The PVC was the last honest argument for a StatefulSet — `volumeClaimTemplates`,
which are the idiomatic way to attach storage to one and which a Deployment
handles badly, deadlocking on a ReadWriteOnce volume under a rolling update.
Removing the volume removes the argument.

## Decision

**1. There is no PersistentVolume and no `persistence.enabled`.** The
controller's spool is an `emptyDir`. ADR 0007's loss analysis stands unchanged;
what is removed is the opt-in it offered alongside it.

This does not forbid durable storage. `spool.dir` is a path: an operator who
mounts something durable there gets exactly the behaviour the opt-in promised,
without a line of code from us. What is gone is our option, our promise about it,
and the second path through the code — not the possibility.

**2. Identity always lives in the Secret.** Strict mode is withdrawn. There is
one identity implementation, the one ADR 0008 §1 and §2 describe. A write-free
installation still exists, but as a consequence rather than a toggle: an
installation that ships nothing to the backend holds no identity and needs no
grant. That is a truthful statement about the agent, where the toggle was a
statement about a chart nobody had written.

**3. The controller is a `Deployment` with `strategy: Recreate`, one replica.**
Recreate closes the case that occurs on every single upgrade — the overlap of the
rolling update — for the price of one line and a few seconds of receiver
downtime, which nodes already retry through.

The node-partition case stays open, deliberately. Closing it costs cluster-wide
collection downtime, and the state it would protect is either loss-harmless or
protected better by point 4.

**4. The identity write is conditional.** When the write of ADR 0008 §1 is built
it issues `update` with the `resourceVersion` read in the same reconcile, and
treats a conflict as a normal outcome: re-read, and if an identity is now there,
adopt it rather than enrolling again. This is recorded here, before that slice
starts, because it is the actual replacement for "one writer, no races" and must
not be rediscovered afterwards.

## Consequences

**Easier.** One storage story instead of two, one identity story instead of two,
and no installation-shaped branch running through the chart, the RBAC table and
the security document. The slice that builds identity gets smaller. A reader of
`security.md` no longer meets a toggle that changes the RBAC table, which was the
one place that document could not state a single fact about access.

**Harder / given up.** There is no supported way to preserve unacknowledged spool
across a pod reschedule during a backend outage. That span is lost, bounded by
the outage length and reported by coverage. This is accepted for the reasons ADR
0007 already gave, and reversing it later is additive: adding a durability option
breaks nothing, which is the cheap direction.

The chart is no longer describable as write-free in every configuration. Once
identity ships, an installation that talks to the backend holds `get`/`update` on
one named Secret. `security.md` discloses it in the table that grants it; the
change is that it is disclosed unconditionally.

**Not changed.** Nothing about what is collected, filtered, or shipped. No
payload byte moves, no configuration field the agent actually reads is added or
removed, and `spool.dir` keeps its meaning. `deploy/controller.yaml` gains one
line.

**Not addressed here.** Writing this exposed a defect that "one writer" was
hiding rather than causing: the sequence counters that order the superseding
kinds are process-local (`var metadataSeq int64`, and its siblings), so they
restart at one on every controller restart. The delivery contract says order
comes from the sequence and never from arrival time, which means a restarted
controller emits payloads the backend must consider older than what it already
holds. Two concurrent controllers make it worse, but a single one already has it.
The fix belongs to the slice that builds shipping, and it is a decision about the
protocol — a monotonic source, or an epoch alongside the sequence — not a patch.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
