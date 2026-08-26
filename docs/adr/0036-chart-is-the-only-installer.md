# 0036. The Helm chart is the only installer, and the install profile is the only switch

Date: 2026-08-26

Status: Accepted
Amends: 0025 §1

Replaces the raw manifests in `deploy/` with a chart in `charts/runtime-agent`.
Adds the `inventory` install profile, which existed as a deployable artifact and
had no name. No change to any payload, to what is collected, or to the agent's
RBAC.

## Context

The repository shipped three manifests: a controller, a node running the
Go-binary scanner, and a node running the scanner plus the eBPF profiler. Each
carried a header saying it was a reference for a chart that did not exist yet,
and the two node files said outright that they were alternatives sharing names —
apply one or the other, never both.

They were two things at once: what an operator was meant to copy, and what the
e2e suite parsed by hand. Nothing checked that those two readings stayed the
same, and the duplication was not abstract.

**Three strings had to agree and nothing made them.** The audience the node's
projected token requests, the audience the controller requires, and the subject
the controller pins are one fact written in three places — and the third
contains a namespace, hard-coded as `runtime-agent`. Installing anywhere else
produced a node whose reports were rejected, with nothing in either manifest
hinting at the coupling.

**The two node files were one file copied.** Their difference is two
capabilities, one read-only mount and one flag; everything else — the zero-RBAC
identity, the token projection, the host PID namespace, the security context —
was maintained twice by hand.

**A documented profile had no artifact and an artifact had no profile.**
`docs/security.md §3` named `metrics-only`, `pprof` and `ebpf`. The
scanner-only node is none of those: it is not `metrics-only`, which has no
DaemonSet, and it is not `ebpf`. It was deployed by the e2e and installable by
anyone reading the repository, under no name.

**And one file demonstrated the trap it was supposed to avoid.** The shipped
eBPF sample enabled the profiler with `allowedModulePrefixes: []`. An empty
allow-list admits no stack frame, so that install profiles a cluster, filters
everything out, and ships nothing — forever, silently. ADR 0025 had already
found this exact shape in `eligibleNamespaces`, called it a knob that reads as
deny-by-default and is inert, and removed it. The sample reintroduced it in the
one field where the consequence is invisible.

## Decision

**1. The chart is the only installer, and `deploy/` is deleted.** Not
generated from the chart, not kept as a reference: deleted. A second artifact
describing the same install is a fact kept in two places, and this repository has
already paid for that once (ADR 0022, on the payload registry).

**2. The e2e installs the chart.** Every suite renders `charts/runtime-agent`
with values and applies the result. What the tests still change is what a
cluster forces them to — an image that exists only in kind, intervals shortened
so a test finishes, and a shell sidecar because the agent image is distroless
and cannot serve its own spool. The endpoints, the RBAC, the capabilities, the
token projection and the configuration are the chart's, unmodified.

This is what makes the chart load-bearing rather than documentation: a template
that breaks fails CI, in the same run as the code.

**3. The install profile is the only switch, and `inventory` is now a
profile.** `metrics-only` installs the controller alone; `inventory` adds the
node running the scanner; `ebpf` adds the profiler to that node. Each is a
superset of the one before, and both halves of a profile — whether the receiver
opens a port, whether the controller answers targeting queries, whether the node
gets the eBPF flag — are derived from it. There is no second value that
half-enables a component.

Naming `inventory` is not an addition to the product. It names what was already
deployable and already deployed, so that a reader of `docs/security.md §3` can
find the privileges it costs.

**4. `pprof` is refused rather than accepted.** It is in `docs/security.md` as
`[planned]`, and no puller exists. The chart fails to render it, with a message
saying so. A value the agent cannot honour must not look honoured — the same
rule ADR 0025 applied to the node's schema, applied one level up to values.

**5. The profiler will not install with an empty allow-list.** Rendering
`ebpf` with no `allowedModulePrefixes` is an error naming the value to set. This
is the one rule the chart adds that no manifest ever enforced, and §Context says
why it is needed.

**6. Values are strict, and some knobs are deliberately absent.**
`values.schema.json` rejects unknown keys, for the reason the agent's own config
parser does: a typo must not silently disable a filter. Absent by decision, not
by omission: spool persistence (ADR 0026 — the spool is always an emptyDir),
replica count (ADR 0008 — one writer), the update strategy, any second namespace
list for profiling (ADR 0025), and anything a backend could set (ADR 0001).

**7. The promises `docs/security.md` makes about an installation are asserted
against the rendered output.** `internal/chartrender/chart_test.go` renders every
profile and checks: no write verb in the ClusterRole; every cache the agent opens
is granted; the node ServiceAccount is named by no binding and mounts no API
token; no privileged container, and exactly the capabilities each profile
promises; every host mount read-only; the spool an emptyDir; and the rendered
configuration parsed by the agent's own strict parser.

That last one matters more than it looks. The agent rejects an unknown
configuration key at startup, so a chart that renders a misspelled key produces
a CrashLoopBackOff in a customer's cluster with the chart looking innocent. The
parser used by the test is the parser used by the agent, so the failure lands in
CI instead.

**8. Names are fixed, except where two installs would collide.** The two
components are `runtime-agent-controller` and `runtime-agent-node` regardless of
release name: the controller pins the node's subject by name and
`docs/security.md` prints that exact string. Cluster-scoped objects carry the
release namespace, so two installs in two namespaces — or two concurrent e2e
runs — do not fight over one ClusterRole.

**9. The chart stays `apiVersion: v2`.** Helm 4 is current and is what this
repository renders and tests with, but the chart is installed by the customer,
with whatever Helm they already have. v2 renders under both, and nothing here
needs a feature newer than that.

## Consequences

**Easier.** An install is one command with one switch, and installing into a
namespace other than `runtime-agent` works — which it did not before. The
security document's claims about an installation are now checked against the
thing that performs the installation, every run, for every profile.

The two node manifests became one template, so the `ebpf` variant's claim — that
it adds exactly two capabilities and one read-only mount over the scanner — is
now visible as a diff of a few lines rather than a diff of two files.

**Harder / given up.** `kubectl apply -f` is no longer an installation path; a
customer without Helm must render the chart first. That is a real cost, and it
buys the single source of truth the manifests could not provide.

The chart's guardrail tests add helm as a dependency of `make test`. It is
wired as a Go tool, like `kind` already is, so no separate install is needed —
at the price of a large module in the dependency graph.

**Not changed.** No payload, no collection behaviour, no RBAC rule. The
`inventory` profile installs exactly what the scanner-only manifest installed,
and `ebpf` exactly what its manifest installed, minus the empty allow-list that
would have made it produce nothing.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
