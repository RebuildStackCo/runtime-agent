# 0055. The installation is cluster-wide, and there is no narrower mode

Date: 2026-08-30

Status: Accepted

Amends: 0032

Removes the namespace-scoped installation mode from `docs/security.md`. It was
never built, never decided, and the document's description of what it would
deliver was wrong. No code changes.

## Context

`docs/security.md` has offered a "namespace-scoped **[planned]**" installation —
Role and RoleBinding in listed namespaces, no ClusterRole — since the first
commit that created the document. No ADR ever decided it. Nothing was ever built
behind it. [ADR 0032](0032-cluster-policy.md) then twice asserted that the mode
"remains available", which took a sentence of speculative prose and gave it the
standing of a decision.

The description was also false, in the direction that matters. It said the mode
"reports requests-vs-usage findings for the permitted namespaces only". The agent
reads every kubelet through `get nodes/proxy`, which is cluster-scoped. Without a
ClusterRole there is no `nodes/proxy`, so there is no usage collection at
all — not reduced, absent. `nodes`, `namespaces`, `priorityclasses` and
`storageclasses` are cluster-scoped for the same reason.

So what the document described as a mode of this product is a different and much
weaker product, offered to a reader who would discover the difference only after
installing it.

## Decision

**1. There is one installation shape: cluster-wide.** The section is replaced by
a statement of that, with the reason — half of what the agent reads is
cluster-scoped — rather than by a narrower promise.

**2. ADR 0032's two references to the mode are void, and this is their
forwarding address.** An accepted ADR's body is immutable
([`README.md`](README.md)), so the sentences there stand as written and are
answered here: the mode they call "available" does not exist and will not.

**3. The scoping mechanism is the four controls, and that is a real answer, not
a fallback.** Namespace filters and the three opt-out annotations apply at the
collection stage, and since [ADR 0054](0054-coverage-says-how-much-was-hidden-never-what.md)
their effect is checkable from what the backend receives rather than only from
what was configured.

The argument for API-server enforcement is that a customer need not trust the
agent's own filtering. It does not survive contact with the rest of the product:
that customer is already trusting agent-side filtering for the symbol allow-list
that governs what leaves a profile, for the drop of `env`, `args` and `command`
before the informer cache, and for the redaction in every node report. And under
the `inventory` and `ebpf` profiles the agent runs with `hostPID` and
`CAP_SYS_PTRACE`, which no namespace-scoped RBAC bounds — the boundary would look
like a guarantee exactly where it is not one.

## Consequences

**Easier.** One fewer unbuilt promise, and one fewer shape for every future
decision to be checked against — ADR 0032 had to weigh what a mode nobody had
built would lose. `security.md` states one installation and describes it truly.

**Harder / given up.** A buyer whose requirement is a namespace boundary enforced
by the API server is not served by this product. That was already true; what
changes is that they learn it from the document instead of from an installation
that collects nothing. If such a buyer ever becomes the reason to build
something, it is a new product decision with its own ADR, not the resumption of
this one.

**Not changed.** No code, no chart, no RBAC. The ClusterRole the chart renders
today is what it rendered yesterday, and `internal/chartrender/guardrail_test.go`
still holds its resource list closed.

This ADR records a documentation change made in the same pull request, per the
process in [`README.md`](README.md).
