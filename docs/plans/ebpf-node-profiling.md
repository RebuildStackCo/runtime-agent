# Development plan — eBPF CPU profiling on the node role

Living plan and shared source of truth for the slices that implement ADR 0011.
Not normative like an ADR: it records *how* we build, slice by slice, so parallel
work does not diverge. When a slice ships, tick it and correct this file to match
what actually landed.

- Design authority: [ADR 0011](../adr/0011-node-ebpf-cpu-profiling.md).
- Customer contract touched: [`security.md`](../security.md) §7.2 (privileges),
  §8 (what leaves the cluster).
- Background: [`spikes/ebpf-pgo.md`](../spikes/ebpf-pgo.md) — eBPF feeds flame
  graphs/coverage, **not PGO** (PGO deferred).

## Invariants every slice must hold (never "temporarily" bend)

1. **One-way to the backend (ADR 0001).** Nothing the external backend returns
   changes agent behavior.
2. **Node holds zero Kubernetes API access (ADR 0009/0010).** No RBAC, default
   token not mounted; the only credential is the controller-audience projected
   token.
3. **No symbol leaves the node before filtering — including logs (constraint a).**
   Until the on-node allow-list filter runs, a captured profile exists *only in
   process memory*. Counters and status may leave; function names may not, not in
   payloads and not in the structured log (logs egress via the customer's log
   pipeline).
4. **Targeting is config-bounded (ADR 0011 §3).** The node's ConfigMap defines the
   eligible set and every ceiling; the controller may only prioritize within it,
   never widen it.
5. **No `privileged`.** The `ebpf` profile adds exactly `CAP_BPF` + `CAP_PERFMON`
   and one read-only `/sys/kernel/btf` mount, nothing more.
6. **Loss-harmless spool + sink invariant (ADR 0003/0007).** The local sink writes
   the exact bytes the backend sink transmits; golden tests are the gate.
7. **Only aggregate counts of dropped/filtered items leave, never their
   identities (CLAUDE.md invariant 6).**

## Cross-cutting constraints and which slice owns each

- **(a) No symbol leaves the node, incl. logs** — enforced in **slice 3** (capture
  output is counters/status only) and **slice 4** (the filter). Slice 3's PR must
  state literally: *before filtering, the profile exists nowhere but process
  memory*, and a test must assert the capture path emits no function names to the
  log.
- **(b) Kernel version is a proxy, not truth** — **slice 2** distinguishes
  `kernel_too_old` vs `btf_absent` as separate reasons/counters; **slice 3** treats
  a failed eBPF program load as an equally graceful refusal (`program_load_failed`
  — LSM/lockdown/`perf_event_paranoid` can fail after Probe passed). The RHEL
  backport case (BTF on 4.18/5.14, version < 5.8) is refused on purpose by the
  current ADR; the split counters tell us what to relax when a RHEL customer
  appears.
- **(c) Slice 7 needs two e2e modes** — a gate-refusal target that runs anywhere
  (kind on macOS: gate fired, counter incremented, scanner still alive) and a
  full-capture target gated to Linux+BTF with a clear skip.
- **Forward (decided, lands in slice 6):** `-enable-ebpf` stays a master on/off
  switch; the eligible set + ceilings arrive **additively** in the node ConfigMap
  when the node role learns `-config` in slice 6. The manifest interface changes
  once (add `-enable-ebpf` in slice 2, add `-config` in slice 6), never twice.

## Slice ordering

1 → 2 → 3 → 4 → 5 → 5.5 → 6 → 7. Each slice is one PR with tests. `security.md`
changes only when a slice changes *what* is collected/transmitted (mainly 3–4).

---

## Slice 1 — ADR 0011 + security.md §7.2/§8 + spike  ✅ MERGED (#25)

Design gate. Docs only. Locks: embed the OTel profiler (no own profiler),
on-node symbolization, `CAP_BPF`+`CAP_PERFMON`/no-privileged/kernel-5.8+,
config-bounded targeting, on-node symbol filtering, validation, spool payload
`source=ebpf`, GPL-2.0 isolation, PGO deferred.

---

## Slice 2 — kernel/BTF gate + capabilities

**Goal.** The node can be deployed in the `ebpf` profile and gates on the kernel:
on `< 5.8` or missing BTF it refuses gracefully with a distinct reason + counter,
the scanner keeps running, and `privileged` never appears.

**In scope.** The gate probe; wiring behind a master switch; the ebpf manifest.
**Out of scope.** The profiler library, capture, reporter, filtering, spool — no
profile is produced in this slice.

**Files.**
- new `internal/ebpfgate/probe.go` + `probe_test.go` (+ `testdata/` osrelease
  fixtures if useful)
- `cmd/agent/node.go` — add the switch and the startup gate
- new `deploy/node-daemonset-ebpf.yaml`
- `deploy/node-daemonset.yaml` — correct the absolute "no eBPF capabilities"
  comments to be variant-dependent

**Types & signatures.**
```go
package ebpfgate

type Reason string
const (
    ReasonSupported    Reason = "supported"
    ReasonKernelTooOld Reason = "kernel_too_old" // < 5.8
    ReasonBTFAbsent    Reason = "btf_absent"     // /sys/kernel/btf/vmlinux missing
    // slice 3 extends: ReasonProgramLoadFailed = "program_load_failed"
)

type Result struct {
    Reason        Reason
    KernelVersion string // e.g. "6.8.0-1064-gcp" (raw osrelease)
    Major, Minor  int
    BTFPresent    bool
}
func (r Result) Supported() bool { return r.Reason == ReasonSupported }

// Probe reads <procRoot>/sys/kernel/osrelease for the kernel version and checks
// <sysRoot>/kernel/btf/vmlinux for BTF. Pure/deterministic given the two roots,
// so tests point them at fixtures. Order: version first (kernel_too_old wins),
// then BTF (btf_absent).
func Probe(procRoot, sysRoot string) Result

// parseKernelVersion extracts leading major.minor from an osrelease string.
func parseKernelVersion(osrelease string) (major, minor int, ok bool)
```

**Config & flags.** `-enable-ebpf` (bool, default false) — master switch.
`-proc` already exists (reused for osrelease). `-sys` (string, default `/sys`) —
root for the BTF check, so the DaemonSet mount path is configurable and tests can
override it. No config file yet (that is slice 6).

**Wiring (`runNode`).** When `-enable-ebpf` is set: call `ebpfgate.Probe(procRoot,
sysRoot)` once at startup. If not `Supported()`, log one clear line
(`msg="ebpf profile refused" reason=... kernel=...`) and increment a per-reason
in-process counter; continue as scanner. If supported, log `msg="ebpf profile
ready"` (profiler wired in slice 3). Default (switch off): behavior unchanged.
Counter stays in-process this slice — it reaches the report stream only when the
profiler slice adds a consumer (per decision).

**Manifest (`node-daemonset-ebpf.yaml`).** Base DaemonSet plus:
`capabilities.add: [SYS_PTRACE, BPF, PERFMON]`, keep `drop: [ALL]`,
`privileged: false`, `allowPrivilegeEscalation: false`,
`readOnlyRootFilesystem: true`, `seccompProfile: RuntimeDefault`; add read-only
hostPath volume `/sys/kernel/btf` → `/sys/kernel/btf`; args add `-enable-ebpf`.
(Note for slice 3: the profiler may need a writable `emptyDir` for the raw ring
buffer under a read-only rootfs — slice 3's concern, flagged here.)

**Tests.**
- `probe_test.go`: table over osrelease (`5.4.0`, `5.8.0`, `5.15.49-linuxkit`,
  `6.8.0-1064-gcp`, `4.18.0-rhel`, malformed/empty) × BTF present/absent → expected
  `Reason` and `BTFPresent`. Assert `kernel_too_old` beats `btf_absent` when both.
- wiring test: `-enable-ebpf` with a fixture procRoot/sysRoot for an old kernel →
  refusal counter == 1, reason `kernel_too_old`, scanner loop still entered; a
  supported fixture → `ready`, no refusal.

**Acceptance.** Old-kernel/no-BTF deploy of the ebpf manifest logs a distinct
refusal, increments the matching counter, and the scanner keeps reporting.
`privileged` absent; only the two caps + BTF mount added. Base manifest comments
no longer claim an absolute "no eBPF capabilities."

**Holds invariants.** 2 (no API), 5 (no privileged), 7 (counts only). Sets up (b).
**Depends on.** Slice 1.

---

## Slice 3 — profiler integration + reporter (capture)

**Goal.** Link `go.opentelemetry.io/ebpf-profiler` as a library and, for a given
target, capture a CPU profile, symbolizing on the node. Output of this slice is a
profile **in memory** plus counters/status — nothing symbol-bearing is logged or
shipped yet.

**In scope.** Library integration; our reporter; on-node symbolization; a capture
for a hardcoded/self target (real targeting is slice 6); GPL-2.0 isolation.
**Out of scope.** Symbol filtering (slice 4), validation/spool (slice 5),
controller-driven targeting (slice 6).

**Files.** `internal/nodeprofile/` (reporter + capture driver); `go.mod`/`go.sum`
gain the profiler + transitive deps; wire the capture behind the slice-2 gate in
`runNode`; ebpf manifest gains a writable `emptyDir` if required.

**Licensing — by fact, not ceremony.** The **`NOTICE` at repo root is
unconditional**: it records the combined-work licensing the moment the GPL-2.0
profiler enters `go.mod`. A **separate GPL-2.0 directory with its own `LICENSE` is
created only if we actually vendor GPL source into our tree.** If the profiler
arrives purely as a Go-module dependency (its eBPF bytecode embedded upstream via
`go:embed`, no GPL source copied here), there is no GPL code in our tree — an
empty dir with a lone `LICENSE` would *mislead* an auditor, so we do not create
one. ADR 0011 §7 allows this: GPL code *"carried"* in our tree lives in its own
dir — the trigger is carrying it, not depending on it. First task of this slice:
determine which case we are in (`go mod download` + inspect), then act
accordingly and say which in the PR.

**Key points.**
- Reporter consumes the profiler's OTLP-profiles callback and holds the raw
  profile in process memory only.
- **(a)** Capture emits counters/status to the log (samples, functions, duration,
  target ref) — **never function names**. A test asserts no symbol strings in the
  capture log.
- **(b)** eBPF program load is the real gate: a load/attach failure is handled as
  gracefully as slice 2's kernel refusal, reason `program_load_failed`, scanner
  survives.

**Tests.** reporter unit tests with a synthetic OTLP-profiles payload (no kernel
needed); a log-scrubbing test proving symbols never reach the log; load-failure
path returns the graceful reason.

**PR must state.** Literally: *before filtering, the profile exists nowhere but
process memory.*

**Holds invariants.** 3 (no symbol egress incl. logs), 5, 2. **Depends on.** 2.

---

## Slice 4 — on-node symbol filtering

**Goal.** Reduce a captured profile to only allowed frames before it can leave the
node: client module-path allow-list kept; stdlib/`runtime` always kept;
third-party per config, dropped by default; everything else dropped. Only kept
frames + an aggregate count of dropped frames survive.

**In scope.** The filter over the in-memory profile; the allow-list/third-party
config shape (consumed here, sourced from config in slice 6); drop counters.
**Out of scope.** Validation/spool (slice 5), targeting (slice 6).

**Files.** `internal/nodeprofile/filter.go` + tests; config types for the
allow-list/third-party policy (wired to real config in slice 6).

**Tests.** golden/table over a fixture profile: allowed client frames kept;
stdlib/runtime kept; third-party dropped by default and kept when configured;
identities of dropped frames never appear in output, only a count.

**Holds invariants.** 3, 7. This is the security control that makes shipping
profiles defensible — tested as a control, not a formatter. **Depends on.** 3.

---

## Slice 5 — serialize + validate + spool payload + golden

**Reconciliation (found by review, 2026-08-11).** The node has **no durable
spool** (ADR 0008/0009: nothing durable on the node); the spool lives on the
**controller** (`cmd/agent/main.go`), which is also the only component with the
API access to resolve a container to its namespace/workload/image digest (ADR
0009 gives the node zero API access). So a profile's full key is a
**controller-side** fact: the node knows only container ID / PID / capture
interval, and the controller joins the rest via PodWatcher — exactly the ADR 0010
inventory pattern. That node→controller **transport + join** is a distinct step,
split out into slice 5.5 below. This slice builds the pieces that stand alone.

**Goal.** Turn a filtered profile into validated, spoolable pprof bytes, and give
the controller a writer for it.

**In scope (all unit-testable in isolation now).**
- `internal/nodeprofile/pprof.go`: filtered `[]Sample` → **gzipped
  `cpu/nanoseconds`** pprof bytes. The profiler reports `samples/count`; convert
  to CPU nanoseconds via the sampling period (`value = count × 1e9/rate`) so the
  output matches the documented `cpu/nanoseconds` contract (ADR §5, security §8).
  Deterministic function/location ordering.
- `internal/nodeprofile/validate.go`: parse; require a `cpu/nanoseconds` sample
  type; require a service function (not `runtime.*`, not the `[filtered]`
  placeholder) among the top functions by value; else invalid + reason, never
  ship.
- `internal/sink/spool.go`: `profilePayload{Kind:"ebpf_profile", namespace,
  workload, container, image digest, capture start/end, source:"ebpf",
  pprof []byte}` + `Spool.WriteProfile(key, pprof)`. Adds `github.com/google/pprof`.
- Golden `internal/sink/testdata/profile.golden.json`.

**Out of scope.** Node→controller transport + join (slice 5.5), the live
capture→filter→serialize→validate→ship pipeline and its cadence (slice 6),
targeting (6), e2e (7).

**Key window is the capture interval, not a wall-clock bucket — decided.** The
window in the key is the specific capture's `start–end`, so **every capture has a
unique key** and no capture is silently superseded. A wall-clock bucket would let
rotation's 2nd/3rd capture of a workload overwrite the 1st under the same-key
supersede rule — silent loss. Profiles use **per-capture filenames**
(`profile-<ns>-<workload>-<container>-<start>-<end>.json`), NOT the superseding
fixed-name pattern; the `maxAge` sweep (ADR 0003/0007) bounds their number.

**Golden stability.** gzip output is not guaranteed stable across toolchains, so
the **serializer is tested by decode-and-assert** (sample type, functions,
values), and the **spool golden uses a fixed pprof byte fixture** (fixed key +
fixed bytes) — the golden guards the payload wrapper (invariant 2), not gzip.

**Tests.** serialize round-trips to a `cpu/nanoseconds` profile with the right
functions/values; invalid profiles (bad sample type / only `runtime.*` / not
parseable) rejected with a reason; `WriteProfile` with a fixed key + fixed bytes →
one per-capture file, golden byte-match; sweep bounds count (no supersession).

**Holds invariants.** 6 (sink invariant/golden), 7. **Depends on.** 4.

---

## Slice 5.5 — node → controller profile transport + join

**Goal.** Ship a validated, filtered profile from the node to the controller
(node-initiated, like ADR 0010) and have the controller enrich the key and spool
it. This is where a profile actually leaves the node.

**In scope.** A node→controller profile endpoint (separate from the 0010
inventory receiver, same projected-token auth); the node ships the validated
pprof bytes + the **node-known key** (container ID, PID, capture interval,
`source=ebpf`) — only allow-list-filtered bytes leave (slice 4); the controller
**joins** container ID → namespace / workload / image digest via PodWatcher (as
0010 does for inventory), fills the full key, and calls `WriteProfile` (slice 5).
An unjoined profile (informer lag, filtered-out pod) is counted and dropped, never
guessed (0010 §5).
**Out of scope.** The capture cadence / rotation (slice 6), e2e (7).

**Holds invariants.** 1 (backend edge untouched), 2 (node zero API), 3 (only
filtered bytes leave), 6. **Depends on.** 5.

---

## Slice 6 — targeting: node asks controller

**Goal.** Node-initiated, config-bounded targeting. On its own interval the node
POSTs a targets query to a new controller endpoint; the controller answers top-N
workloads by consumption from its rollups; the node profiles within its config's
eligible set and ceilings.

**In scope.** The new controller endpoint (separate from the 0010 inventory
receiver, same projected-token auth); the node query client; the node role learns
`-config` and reads the `Profiling` config (allow-list, third-party policy, N,
capture duration default 60s, frequency, rotation, overhead ceiling); the counter
from slice 2 now reaches the report stream (a consumer exists).
**Out of scope.** e2e (slice 7).

**Files.** `internal/config` (`Profiling` struct + defaults); node query client;
controller targets handler reading a **published top-N snapshot** (see below);
`cmd/agent` wiring on both roles; ebpf manifest gains `-config` + a ConfigMap.

**Concurrency — do not race the Accumulator.** The usage `Accumulator` is owned
by the poller goroutine and has **no synchronization by design** (documented in
its code). The targeting HTTP handler MUST NOT read it directly (data race), and
we MUST NOT bolt a mutex onto the Accumulator (that breaks its single-owner
design). Correct pattern: the poller, on its own tick, computes and **publishes a
top-N snapshot into an `atomic.Pointer[[]Target]`** (or an equivalent
copy-on-publish value); the handler reads only the published snapshot. The
snapshot is a small, already-ranked, already-filtered slice — never the live
rollup state. A test asserts the handler never touches the Accumulator.

**Invariant 4 is the whole point.** The reply can only prioritize within the
ConfigMap-defined eligible set and cannot exceed any ceiling; a test asserts a
reply naming a non-eligible workload (or exceeding N) is ignored. Reuse ADR
0011's honest-tradeoff framing in the PR.

**Tests.** query/response round-trip with a fake controller; config bounds honored
(non-eligible target ignored, ceilings clamp); rotation and overhead ceiling.

**Holds invariants.** 1 (backend edge untouched), 2, 4. **Depends on.** 5.

---

## Slice 7 — e2e in kind (two modes)

**Goal.** Prove the path end to end where the kernel allows, and prove the
graceful refusal where it does not.

**In scope.** Two `//go:build e2e` targets with env-var + kernel/BTF skips:
- `profile-gate-e2e` — runs anywhere (incl. kind on macOS/linuxkit): deploy the
  ebpf manifest, assert the gate refused (`kernel_too_old`/`btf_absent`), the
  counter incremented, and the scanner still reports. A real, valuable e2e, not a
  stub.
- `profile-capture-e2e` — Linux + BTF only: CPU-load pod → profile captured,
  filtered, validated, present in the spool; `t.Skip` with a clear message on
  kernels without BTF (probe `/sys/kernel/btf/vmlinux`).
**Out of scope.** none beyond the above.

**Files.** `test/e2e/profile_gate_test.go`, `test/e2e/profile_capture_test.go`;
Makefile targets; extend the existing DaemonSet-apply harness for the ebpf
manifest.

**Note.** Unit tests of the wrapper (gate, filter, validate, payload) are
mandatory regardless and already land in slices 2–5; e2e is additive proof.

**Holds invariants.** all. **Depends on.** 6 (full path) / 2 (gate mode).

---

## PR checklist (every slice)

- [ ] Tests for the new behavior; `make test` (`go test -race ./...`) green.
- [ ] `make lint` clean.
- [ ] `security.md` updated **iff** the slice changes what is collected/transmitted.
- [ ] No symbol identity added to any log or payload before slice 4's filter.
- [ ] No `privileged`; only the two caps + BTF mount in the ebpf manifest.
- [ ] Counters distinguish reasons where (b) requires it.
- [ ] Conventional Commit; PR body states the slice's required invariant claims.
