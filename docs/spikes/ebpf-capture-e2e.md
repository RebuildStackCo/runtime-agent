# Spike / test report: eBPF capture end-to-end on a real kernel

Engineering spike feeding ADR 0011. Not product code. It answers the question the
gate-refusal e2e cannot: *does the node eBPF profiler actually capture, symbolize,
and allow-list-filter a real workload on a real kernel — and at what privilege?*
The throwaway proof harness (a small program importing `internal/nodeprofile`) is
kept out of tree; the method below is enough to reproduce.

## Verdict

Two findings, one good and one that changes ADR 0011:

1. **The capture data-path is correct on real eBPF.** With a profiler that actually
   loads, our `internal/nodeprofile` chain — capture → on-node symbolize → symbol
   filter → serialize → validate — does exactly what ADR 0011 §4–5 promise: the
   workload's own allow-listed frames survive, every third-party frame is redacted
   to `[filtered]`, and the result is a valid `cpu/nanoseconds` pprof.

2. **The upstream profiler needs `privileged: true` to capture on this kernel.**
   ADR 0011's headline — *"exactly two added capabilities (CAP_BPF, CAP_PERFMON),
   no `privileged`, minimal auditable surface"* — does **not** hold. A restricted
   capability set (even a generous one with all LSM profiles relaxed) loads the
   profiler but collapses its sampling to a few percent and symbolizes nothing.
   Only a fully privileged container captured the workload.

## Environment

A live run was done on a GCP VM (Ubuntu 22.04, kernel **6.8-gcp**, with
`/sys/kernel/btf/vmlinux` present; VM deleted after). The node's own container
image and the `test/e2e/sample` workload (made CPU-hot: `busywork.Grind` loops on
its own arithmetic and calls `github.com/cespare/xxhash` from the hot path) were
used unmodified. The proof harness starts the real `nodeprofile` capture, drains a
window, runs it through the real filter/serialize/validate, and reports which
frames survived.

## What was tested, and results

### 1. Data-path correctness (bare host, and a plain privileged container) — PASS

Running the profiler as a privileged process/container against the CPU-hot
workload, over a 25 s window at 99 Hz:

```
RAW:      samples=5381  distinct_funcs=1689  own_frames=4891  third-party(xxhash)_frames=2
FILTERED: own_frames=4891  third-party_frames=0  redacted("[filtered]")_frames=…  thirdPartyDropped=…
VALIDATE_OK: cpu/nanoseconds profile
own-module functions kept:
  - example.com/rebuildstack-e2e/goworkload/busywork.Grind   (4891 frames)
```

So on a real kernel the real profiler symbolizes correctly, the allow-listed
own-module frame (`busywork.Grind`) survives, all third-party frames (thousands,
across the whole system) are redacted, and the serialized profile passes our
`Validate` (cpu/nanoseconds sample type + a service function among the top). This
is the core capability of ADR 0011, proven on live data.

### 2. kind cannot run the profiler — the capture e2e must skip there

Inside a `kind` node (a nested container), the profiler's own start-up "system
analysis" step fails regardless of privilege or sysctls:

```
program_load_failed: … failed to determine system configs:
  system analysis request was not handled for pid <pid> at 0x20
```

The eBPF *gate* (kernel version + BTF) passes and the pod starts, but the tracer
never loads, so no profile is ever produced. A plain `docker run` on the same host
(not nested in kind) works — so containers per se are fine; **kind's nesting is the
wall.** Consequence: `TestEBPFCaptureEndToEnd` can never assert inside kind and must
**skip** when the node logs `program_load_failed` (otherwise it hangs to timeout).
kind still covers the graceful-refusal path; full capture needs a non-kind host.

### 3. Capability chain — the promised two are insufficient

On a hardened kernel (`kptr_restrict=1`, `perf_event_paranoid=4`) the profiler
loads only after a chain of *additional* capabilities, each a distinct load-gate:

| Missing capability | Failure it produces |
|---|---|
| `CAP_SYSLOG`       | `unable to read kallsyms addresses` (kernel-symbol read blocked by `kptr_restrict`) |
| `CAP_SYS_RESOURCE` | `failed to adjust rlimit: … operation not permitted` (raise `RLIMIT_MEMLOCK` for eBPF maps) |

So beyond `CAP_BPF` + `CAP_PERFMON` (+ `CAP_SYS_PTRACE`, already held by the
scanner), the profiler also needs `CAP_SYSLOG` and `CAP_SYS_RESOURCE` merely to
*load*. `CAP_PERFMON` did correctly bypass `perf_event_paranoid`, so the paranoid
level itself was not a gate once that capability was present.

### 4. …but even a generous capability set does not capture — only `privileged` does

A clean, controlled 3-way comparison (same freshly-started workload for each mode,
same host, same window):

| Container security mode | samples | own-module (`Grind`) frames | result |
|---|---:|---:|---|
| `--privileged` | 5381 | 4891 | ✅ full capture |
| `cap-drop=ALL` + `BPF,PERFMON,SYSLOG,SYS_RESOURCE,SYS_PTRACE` | 148 | 0 | ❌ collapsed |
| the 5-cap set **+ seccomp, AppArmor, systempaths all unconfined** | 198 | 0 | ❌ collapsed |

With the restricted capability set the profiler *loads* and emits a *valid* profile,
but samples drop by ~97 % and the target workload is never symbolized. Relaxing
seccomp, AppArmor, and the masked `/proc` paths did **not** restore it. The gap
between `privileged` and the restricted set is therefore **not** capabilities, not
seccomp, not AppArmor, and not `/proc` masking — each was tested and ruled out. On
this kernel the upstream profiler effectively requires `privileged`.

## Implications for ADR 0011

- The capture, on-node symbolization, and symbol-filter design are **validated** —
  the data that would leave the node is correct and correctly redacted.
- The security posture is **not** as promised. Running this profiler means
  `privileged: true`, not a small auditable capability delta. The other guarantees
  (zero RBAC, read-only root filesystem, no control channel, symbol filtering)
  still hold, but "minimal privilege" does not.
- Test strategy: the full-path capture e2e cannot run in kind and must skip there;
  it asserts only on a non-kind Linux host / real cluster. kind CI keeps the
  gate-refusal e2e.

Open decision (out of scope for this report): pivot ADR 0011 to a `privileged`
opt-in profile and document the posture honestly; pause the feature because the
minimal-privilege promise was its premise; or investigate a non-eBPF / alternative
profiler that works unprivileged.

## Method (reproducible, no local paths)

1. On a Linux host with kernel ≥ 5.8 and `/sys/kernel/btf/vmlinux` present, build a
   small program that imports `internal/nodeprofile`, calls `nodeprofile.Start`,
   sleeps one window, `Drain()`s, then runs the samples through
   `NewSymbolFilter([...own module prefix...], ThirdPartyDrop)`, `Serialize`, and
   `Validate`. Have it print, per function, how many frames are own-module vs.
   contain the third-party marker vs. the `[filtered]` placeholder.
2. Run the CPU-hot `test/e2e/sample` workload as a separate process so the
   system-wide profiler has a busy, symbolizable target.
3. Run the harness three ways and compare sample counts and surviving frames:
   `--privileged`; `--cap-drop=ALL` plus `CAP_BPF,CAP_PERFMON,CAP_SYSLOG,`
   `CAP_SYS_RESOURCE,CAP_SYS_PTRACE`; and that same cap set plus
   `--security-opt seccomp=unconfined,apparmor=unconfined,systempaths=unconfined`.
   Mount host `/sys/kernel` read-only and share the host PID namespace, mirroring
   the DaemonSet.
4. For the kind observation: deploy the `ebpf` node variant into a kind cluster and
   read the node log — the gate reports ready, then the tracer fails with
   `program_load_failed: … system analysis request was not handled`.
