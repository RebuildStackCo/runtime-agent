# 0025. Profiling scope is collection scope; the node's config holds only what the node enforces

Date: 2026-08-23
Status: Accepted
Amended by: 0036
Amends: 0011 §3, §4, 0022 §5

Removes the profiling eligible set introduced by ADR 0011 §3, corrects what that
section claims bounds a compromised controller, and corrects §4 on the scope of
the symbol allow-list. Closes the fourth of the five items ADR 0022 §5 recorded
as open. ADR 0011's other decisions — the embedded profiler, the capabilities and
kernel gate, on-node symbolization, node-side symbol filtering, validation before
spooling, GPL isolation — are unchanged.

## Context

ADR 0011 §3 justified the one node↔controller reply the node acts on by bounding
it:

> **the node's ConfigMap defines the eligible set and every ceiling** … the worst
> a rogue or compromised controller can do is reorder already-permitted targets
> within already-configured limits.

That sentence is the entire argument for departing from ADR 0010 §1's ack-only
reply discipline. Examined, almost none of it held.

**The eligible set was never enforced on the node.** `targeting.Publisher`
filtered by `eligibleNamespaces` on the controller, from the *controller's*
ConfigMap. The node read the same field from its own file and did nothing with it
but log its length.

**The node's ConfigMap advertised the knob anyway**, as
`eligibleNamespaces: []` under the comment "which workloads may be profiled at
all (empty = none)". Setting it there changed nothing. Relative to the node's own
configuration the behaviour was fail-open: the empty list that reads as "profile
nothing" was inert. Both roles parsed one `config.Config`, so the field was
structurally valid and silently ignored — as were `filters`, `spool` and
`nodeIntake` in a node ConfigMap.

**And the promised bound is not achievable by that design.** Suppose the node did
re-check. The reply carries container IDs; to check a namespace the reply would
have to carry the namespace, and that namespace is then a claim the controller
made. A hostile controller wanting a container in `secret-ns` labels it
`payments`. The node resolves a process to a pod UID through its cgroup, never to
a namespace, and holds zero Kubernetes API access by construction (ADR 0009).
Every namespace fact reaches it through the controller, so **no node-side check
of a controller-supplied label can constrain the controller** — the same applies
to the scan scope of ADR 0015. A re-check would look like a safeguard and be
none.

**Then the field's real cost showed up.** Nobody would ever fill it in. It asks
an operator to enumerate namespaces a second time, in a second file, in a shape
whose empty value means the opposite of the collection filter's — where an empty
`filters.namespaces.allow` admits everything. `deploy/controller.yaml` proved the
trap by shipping the dead combination:

```yaml
profiling:
  enabled: true
  eligibleNamespaces: []
```

Profiling enabled, nothing eligible, no profile ever produced, and not a line of
log about it. The repository's own manifest was the first victim.

**Separately, profiling ignored the scan scope entirely.** ADR 0015 made the
scanner fail closed against the controller-supplied scope: no scope, no
executable opened. The profiling pipeline never consulted it. The node would
refuse to read a pod's *build information* while capturing that same pod's *stack
traces* if told to — the more sensitive signal held to the weaker standard.

A fourth inaccuracy, smaller: ADR 0011 §4 describes the symbol allow-list as
"configured for the workload". It is one list per node. That matters here,
because it is what survives the analysis above.

## Decision

**1. There is no profiling eligible set.** `eligibleNamespaces` and
`eligibleWorkloads` are removed from both schemas. Which workloads may be
profiled is which workloads are collected — `filters.namespaces` — and a second
namespace list was the same intent expressed twice, with opposite meanings for an
empty value.

The security posture does not weaken, because it never rested there. Deny-by-
default lives where it is already unavoidable: profiling requires deploying a
DaemonSet with `CAP_BPF` and setting `profiling.enabled: true`. Both are
deliberate acts; neither happens by accident. A third switch nobody sets adds a
silent failure, not a boundary.

It also makes the targeting reply structurally what the scan scope already is.
The ranking is computed from usage rollups, which exist only for admitted pods,
and the expansion to container IDs reads `PodWatcher`'s admitted index. A
workload the customer excluded was never measured and is not in that index, so
the controller cannot name it — the same by-construction property ADR 0015 relied
on, now covering both replies.

**2. The two roles have separate configuration schemas.** `config.NodeConfig`
holds only what the node enforces on its own samples: the symbol allow-list, the
third-party policy, and the ceilings on what profiling costs it.

The test of membership is not "does the node read it" but **"can the node enforce
it alone"**. `UnmarshalStrict` turns the old silence into a startup failure: a
controller-only setting in a node ConfigMap now stops the node. A setting the
node cannot honor must not look like one it does.

**3. The node has no target-count limit.** `maxTargetsPerWindow` is removed with
the rotation it existed for. Profiling is directed from the controller, and
`topN` there bounds how many workloads an answer may name; a second count limit
in a second file only raised the question of which one applied.

Removing it does not remove a real bound. The node's CPU cost is set by its
sampling rate, which `overheadCeilingPercent` fixes and a target count does not
change, and is capped absolutely by the container's own CPU limit. The spool a
flood of targets would fill belongs to the controller that asked for them.

The consequence worth recording: with `topN` workloads × their replicas ×
containers on a node, a window can now ship more profiles than before — order
5–15 per node per minute at the default. Bounded, self-inflicted, and adjustable
by `topN`; if it ever bites, that is the knob, not a new one.

**4. Profiling honours the scan scope, and fails closed.** A window's samples
ship only for containers the targets reply names *and* whose pod the scope
admits. With no scope the window ships nothing.

This does not bound a hostile controller either. It is worth doing for a
different reason: it removes the asymmetry where profiling was permitted on a pod
scanning was forbidden, it catches a controller disagreeing with itself across
informer lag, and it gives the more sensitive signal the stricter discipline.
Out-of-scope samples are counted and logged as a number; the pod UID never
appears (CLAUDE.md invariant 6).

**5. The bound is restated honestly.** What constrains a *hostile* controller is
what the node holds itself: the symbol allow-list, the cost ceilings, profile
validation, and the requirement that the container run on this node. What
constrains only a *buggy* one is the collection scope and the targets reply. Both
lists are in `security.md` §7.2 as a table, because the distinction is the
security argument and prose let it drift for four slices.

The symbol allow-list is therefore the load-bearing control of the profiling path
— the reason shipping a stack trace is defensible at all — which is why it stays
in a file Helm owns rather than arriving over the wire.

## Consequences

**Easier.** Three configuration keys are gone and the remaining ones all do
something where they are set. An operator scopes profiling the same way they
scope everything else, once. A security review gets a table of which bounds
survive a hostile controller instead of a claim that does not. The profiling path
inherits ADR 0015's fail-closed discipline rather than being the one place that
skipped it.

**Harder / given up.** There is no longer a way to profile *less* than what is
collected. A customer who wants collection cluster-wide but profiling in one
namespace cannot express that. This is accepted: no such customer exists yet, the
knob that would have served them was one nobody would set, and adding a narrowing
filter later is additive and non-breaking — the reverse of the direction that
costs a migration.

An existing install with `eligibleNamespaces` in either ConfigMap will not start
after this change; the pod fails on config parse. That is the intended shape —
loud beats inert — and it is free today, with no releases cut. It was not free in
the repository's own end-to-end test, whose fixtures carried exactly those fields
and had to be corrected. That is what the failure mode looks like.

Profiling now makes one extra scope request per window per node. At the default
60-second window that is negligible, and it is deliberately a separate fetch
rather than shared state with the scanner: the two run on different cadences, and
a cache between them would be one more thing to reason about for no gain.

**Not changed.** Everything ADR 0011 decided about how profiling works. Nothing
about what is captured, filtered, validated or shipped moves; no payload byte
changes. This decision is about which component is allowed to believe what, and
about not asking an operator for something they will not give.

**Not addressed here.** The controller is a `Deployment` while ADR 0008 derives
"one writer, no races" from a single-replica StatefulSet — the last of ADR 0022
§5's open items, and one for the slice that builds identity.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
