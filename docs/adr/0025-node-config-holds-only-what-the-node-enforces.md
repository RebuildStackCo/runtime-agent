# 0025. The node's configuration holds only what the node can enforce

Date: 2026-08-23
Status: Accepted
Amends: 0011 §3, §4, 0022 §5

Corrects ADR 0011 §3 on where the profiling eligible set lives and what bounds a
compromised controller, and §4 on the scope of the symbol allow-list. Closes the
fourth of the five items ADR 0022 §5 recorded as open. ADR 0011's decisions —
the embedded profiler, the capabilities and kernel gate, on-node symbolization,
node-side symbol filtering, validation before spooling, GPL isolation — are
unchanged.

## Context

ADR 0011 §3 justified the one node↔controller reply the node acts on by bounding
it:

> **the node's ConfigMap defines the eligible set and every ceiling** … the worst
> a rogue or compromised controller can do is reorder already-permitted targets
> within already-configured limits.

That sentence is the entire argument for departing from ADR 0010 §1's ack-only
reply discipline. It is also wrong in three separate ways, and the third is the
one that matters.

**The eligible set was never enforced on the node.** `targeting.Publisher`
filters by `eligibleNamespaces` on the controller, from the *controller's*
ConfigMap. The node read the same field from its own file and did nothing with
it but log its length.

**The node's ConfigMap advertised the knob anyway.** The shipped sample carried

```yaml
profiling:
  # Which workloads may be profiled at all (empty = none).
  eligibleNamespaces: []
```

under a comment promising deny-by-default. An operator setting it there changed
nothing. Relative to the node's own configuration the behaviour was fail-open:
the empty list that reads as "profile nothing" was inert, and profiling
proceeded on the controller's say-so. Both roles parsed one `config.Config`, so
the field was structurally valid and silently ignored — as were `filters`,
`spool` and `nodeIntake` in a node ConfigMap.

**And the promised bound is not achievable by the design it describes.** Suppose
the node did re-check. The reply carries container IDs; to check a namespace the
reply would have to carry the namespace too. That namespace is then a claim the
controller made, and the node cannot test it — a hostile controller wanting a
container in `secret-ns` labels it `payments` and the node admits it. The node
resolves a process to a pod UID and a container ID through its cgroup and never
to a namespace, and it holds zero Kubernetes API access by construction
(ADR 0009). Every namespace fact reaches it through the controller, so **no
node-side check of a controller-supplied label can constrain the controller.**
The same applies to the scan scope of ADR 0015.

A fourth inaccuracy, smaller but in the same family: ADR 0011 §4 describes the
symbol allow-list as "configured for the workload". It is one list per node,
from the node's own file. Which matters here, because it is what actually
survives the analysis above.

**Separately, profiling ignored the scan scope entirely.** ADR 0015 made the
scanner fail closed against the controller-supplied scope: no scope, no
executable opened. The profiling pipeline never consulted it. So the node would
refuse to read a pod's *build information* while capturing that same pod's
*stack traces* if told to — the more sensitive signal held to the weaker
standard.

## Decision

**1. The two roles have separate configuration schemas.** `config.Config` stays
the controller's. `config.NodeConfig` is new and holds only what the node
enforces on its own samples: the symbol allow-list, the third-party policy, and
the ceilings — max targets per window, capture duration, interval, overhead.

The test of membership is not "does the node read it" but **"can the node
enforce it alone"**. The allow-list and the ceilings qualify: the node applies
them to samples it captured, and no reply can widen them. Eligibility does not,
for the reason above.

`UnmarshalStrict` turns the old silence into a startup failure: a
controller-only setting in a node ConfigMap now stops the node. A setting the
node cannot honor must not look like one it does.

**2. `topN` becomes `maxTargetsPerWindow` on the node.** One name meant two
limits — how many workloads the controller may *name*, and how many the node
will *capture*. Both still apply and the effective limit is the smaller, which
is a real defence in depth; naming them alike is how an operator stops knowing
which one took effect.

**3. Profiling honours the scan scope, and fails closed.** A window's samples are
shipped only for containers the targets reply names *and* whose pod the scope
admits. With no scope — no endpoint configured, or a controller that cannot be
reached — the window ships nothing.

This does not bound a hostile controller either; the scope is its answer too. It
is worth doing for a different reason: it removes the asymmetry where profiling
was permitted on a pod scanning was forbidden, it catches a controller
disagreeing with itself across informer lag, and it gives the more sensitive of
the two signals the stricter of the two disciplines. Out-of-scope samples are
counted and logged as a number; the pod UID never appears (CLAUDE.md
invariant 6).

**4. The bound is restated honestly.** What constrains a hostile controller is
exactly what the node holds itself: the symbol allow-list, the ceilings, profile
validation, and the requirement that the container run on this node. What
constrains only a *buggy* one is the eligible set and the scan scope. Both lists
are in `security.md` §7.2 as a table, because the distinction is the security
argument and prose let it drift for four slices.

The symbol allow-list is therefore the load-bearing control of the profiling
path — the reason shipping a stack trace is defensible at all — and this is why
it must stay in a file Helm owns rather than arrive over the wire.

## Consequences

**Easier.** An operator reading the node's ConfigMap sees only settings that do
something there. A security review gets a table that says which bounds survive a
hostile controller instead of a claim that does not. The profiling path inherits
ADR 0015's fail-closed discipline rather than being the one place that skipped
it.

**Harder / given up.** An existing install with `eligibleNamespaces` in the
node's ConfigMap will not start after this change; the pod fails on config
parse. That is the intended shape — loud beats inert — and it is free today,
with no releases cut. It was not free in the repository's own end-to-end test,
whose node fixture carried exactly that field and had to be corrected; the
failure mode is real and this is what it looks like.

Profiling now makes one extra scope request per window per node. At the default
60-second window that is negligible, and it is deliberately a separate fetch
rather than shared state with the scanner: the two run on different cadences,
and a cache between them would be one more thing to reason about for no gain.

**Not changed.** Everything ADR 0011 decided about how profiling works. Nothing
about what is captured, filtered, validated or shipped moves; no payload byte
changes. This decision is about which component is allowed to believe what.

**Not addressed here.** The controller is a `Deployment` while ADR 0008 derives
"one writer, no races" from a single-replica StatefulSet — the last of ADR 0022
§5's open items, and one for the slice that builds identity.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
