# 0011. On-node eBPF CPU profiling: embedded profiler, node-side symbolization, config-bounded targeting query

Date: 2026-08-11

Status: Accepted
Amends: 0009, 0010
Amended by: 0022, 0023, 0025, 0039, 0041, 0058, 0059, 0060

Extends the node role of 0009 and the node→controller channel of 0010; scopes a
narrow, config-bounded exception to 0010 §1's "one level down" reply discipline
for a new targeting endpoint, leaving the inventory channel unchanged; the
agent↔backend one-way boundary of 0001 is untouched. Records the outcome of the
PGO spike: eBPF profiles feed flame graphs and coverage, not PGO.

## Context

The metrics-only and Go-inventory work tells us *which* workloads are expensive
and *what* they are built from. It does not tell us *where inside a workload* the
time goes. For services that already expose `net/http/pprof` we can pull that
natively; for everything else the node role is the only component positioned to
see it, by attaching an eBPF CPU profiler to processes it can already observe
under `/proc` (0009).

A profiling spike (`docs/spikes/ebpf-pgo.md`) settled two things that bound this
decision. First, `go.opentelemetry.io/ebpf-profiler`
attaches and symbolizes Go on a stock kernel with BTF — verified end to end on a
real 6.8 kernel. Second, its output is **not** valid for Go PGO: it emits no
`Function.start_line` and no inline frames, and current Go *fails the build* on
such a profile. **PGO is therefore deferred** (both from-eBPF and from-pprof),
and this ADR scopes eBPF to flame graphs and hot-function attribution — never a
PGO artifact. That promise wording is load-bearing and is repeated in
`security.md` §8.

Four constraints from prior decisions bound the design, and three of them may not
bend:

- **One-way flow to the backend (0001).** The agent initiates every connection
  and nothing the *backend* returns changes agent behavior. This governs the
  agent↔backend edge and is not in question here.
- **The node holds no Kubernetes identity (0009 §4, 0010).** Its ServiceAccount
  is bound to nothing; `automountServiceAccountToken: false`. The one credential
  it may hold is the controller-audience projected token of 0010.
- **On-node filtering (0009 §5).** Identities of filtered-out objects never leave
  the node. For profiles this is the whole game: a stack trace is a map of the
  customer's code structure, and that map must be reduced on the node before any
  byte crosses to the controller.
- **The reply discipline of 0010 §1** — "the receiver's response … never returns
  configuration, tasks, or anything the node acts on" — is the one constraint
  this ADR consciously revisits, for one new endpoint only, below.

The forcing tension is targeting. "Profile the top-N workloads by consumption" is
a *cluster-wide* ranking. The node sees only its own processes; the aggregated
rollups live in the controller. So either the node profiles blindly, or it asks
the controller who is worth profiling — and an answer the node acts on is exactly
what 0010 §1 forbade. We reconcile this rather than route around it.

## Decision

1. **Embed the OTel eBPF profiler as a library; we do not write our own.** The
   node role links `go.opentelemetry.io/ebpf-profiler` and drives it through a
   first-party reporter that hands captured profiles to the existing on-node
   pipeline. **Symbolization happens on the node**: stack addresses are resolved
   against the target's own binary in place, the binary is never copied or
   uploaded, and only symbolized, filtered profiles leave the node — and only
   toward the in-cluster controller.

2. **Capabilities are `CAP_BPF` + `CAP_PERFMON`, never `privileged`, gated on
   kernel 5.8+.** The `ebpf` profile adds these two capabilities on top of the
   scanner's `SYS_PTRACE`, with `drop: [ALL]`, `privileged: false`, and
   `allowPrivilegeEscalation: false` intact. On startup the node checks the
   kernel version and BTF availability; below 5.8, or without the kernel support
   the profiler needs, the `ebpf` profile **refuses gracefully** — it does not
   start the profiler, it reports the condition and increments a counter, and the
   Go-binary scanner role continues unaffected. Unsupported kernels degrade; they
   are never silently escalated to `privileged`.

3. **Targeting is a node-initiated, config-bounded query — a scoped exception to
   0010 §1, not a control channel.** On its own interval the node POSTs a
   *targets query* to a new controller endpoint (distinct from the inventory
   receiver of 0010, same node-initiated projected-token auth). The controller
   answers with the top-N workloads by consumption drawn from its own rollups.
   What keeps this from being a control channel is a hard safeguard: **the node's
   ConfigMap defines the eligible set and every ceiling** — the namespace/module
   allow-list of what may be profiled at all, the cap on N, the capture duration
   (default 60s), the frequency, the rotation, and the overhead ceiling. The
   controller's reply can only *prioritize or select within what the node's
   configuration already permits*; it can never enlarge the eligible set, raise a
   cap, or cause the node to profile something the chart did not allow. The reply
   carries workload identifiers — data derived from the cluster's own metrics —
   and never configuration, code, or a task. Thus the worst a rogue or
   compromised controller can do is reorder already-permitted targets within
   already-configured limits.

   This narrows 0010 §1's blanket "one level down" claim for this one endpoint,
   consciously and with the bound named. The inventory channel of 0010 is
   unchanged and stays ack-only. Invariant 1 of 0001 — the agent↔backend edge —
   is untouched: the external backend still receives nothing it can act on, and
   the node's behavior remains bounded by its own ConfigMap exactly as the
   agent's is bounded against the backend.

4. **Symbols are filtered on the node before egress.** A frame is kept only if
   its Go module path is on the client allow-list configured for the workload;
   standard-library and `runtime` frames are always kept (they carry no client
   structure and make a profile readable); third-party dependency frames are
   governed by config and **dropped by default**. Every other frame is dropped on
   the node. This is the direct answer to the security-review concern that stack
   traces expose code structure: the identities of filtered functions never leave
   the cluster, only the allowed ones and aggregate counts of what was dropped.

5. **A profile is validated before it is spooled.** It is accepted only if it
   parses as a pprof profile, carries a `cpu`/`nanoseconds` sample type, and has
   at least one service (non-`runtime.*`) function in its top. A profile that
   fails any check is dropped, counted, and never shipped — "not sent" beats
   "sent and wrong."

6. **The profile enters the spool as a payload: pprof bytes plus a key.** The key
   is (namespace, workload, container, image digest, time window,
   `source=ebpf`). Delivery follows the existing one-way, at-least-once,
   supersede-by-key/ack semantics of 0003/0007; the local sink writes the exact
   bytes the backend sink transmits, and a golden test over the payload wrapper is
   the enforcement (invariant 2). Raw, unfiltered samples never reach the spool
   (see `security.md` §8).

7. **The GPL-2.0 eBPF components are isolated.** Upstream eBPF code carried under
   GPL-2.0 lives in its own directory with its own `LICENSE`, and a repo-root
   `NOTICE` records the combined-work licensing. The Apache-2.0 license of the
   rest of the agent is unchanged; the isolation and NOTICE make the boundary
   auditable.

## Targeting — the honest tradeoff

The reply the node acts on is a real departure from 0010's stricter posture, so
its residual risk is stated rather than hidden:

- A controller that is compromised or buggy can *bias* profiling: within the
  workloads the node's config already permits, it can steer which ones get
  profiled and therefore which ones carry the sampling overhead. It cannot reach
  outside that permitted set, cannot exceed the configured N, duration, frequency,
  or overhead ceiling, and cannot turn profiling on for a cluster whose chart did
  not enable it.
- Compensating controls: the eligible set and all ceilings live only in the
  ConfigMap (Helm-owned, not writable over the wire); target rotation prevents a
  single steered choice from monopolizing capture; the overhead ceiling caps the
  cost regardless of what the reply asks for; the same NetworkPolicy and audience-
  bound token of 0010 restrict who may answer the query at all.
- If even this bias is unacceptable for a deployment, the node can be configured
  to ignore the controller's ranking and profile its own eligible workloads
  round-robin — a config choice, not a code change. The query is an optimization
  over which permitted workloads to spend a fixed overhead budget on, not the
  source of authority for whether to profile.

## Consequences

Easier:

- CPU profiles and flame graphs become available for workloads that expose no
  pprof endpoint, with no change to customer code and no binary ever leaving its
  node.
- The PGO question is settled and deferred with a written reason, so the profile
  contract can promise flame graphs and hot-function attribution without
  over-claiming (`security.md` §8).
- The node keeps zero Kubernetes API access; the new query reuses 0010's
  node-initiated, audience-bound-token mechanism and adds no RBAC verb.

Harder, or given up:

- The node↔controller edge now has one endpoint whose reply the node acts on.
  Keeping that reply config-bounded — data within the node's own permissions,
  never a command — is a standing obligation, and a future change that let the
  controller widen the node's behavior would break the boundary this ADR drew.
- The node gains `CAP_BPF` and `CAP_PERFMON`: a wider kernel surface than the
  scanner's `SYS_PTRACE`, bounded by no-`privileged`, the 5.8 gate, and the
  overhead ceiling. Kernels without 5.8/BTF cannot run the `ebpf` profile and are
  degraded, reported, and counted rather than failed closed.
- `go.mod` gains the eBPF profiler and its transitive dependencies, and GPL-2.0
  code enters the tree. It is isolated in its own directory with its own LICENSE
  and recorded in NOTICE; the combined-work implication is real and is documented
  rather than absorbed silently.
- Stack traces are the most sensitive stream the agent handles. The on-node
  allow-list filter (point 4) is what makes shipping them defensible; it is a
  security control, not a formatting step, and is tested as one.
