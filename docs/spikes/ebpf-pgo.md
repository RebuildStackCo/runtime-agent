# Spike: is `go.opentelemetry.io/ebpf-profiler` output usable for Go PGO?

Engineering spike feeding ADR 0011. Not product code. The throwaway reproduction
artifacts (test workload, pprof analyzer/ablator, OTLP sink) are kept out of tree;
the method below is enough to reproduce.

## Verdict

**Invalid as-is for PGO.** An eBPF profile from `go.opentelemetry.io/ebpf-profiler`
cannot drive Go PGO, and on current Go it does not merely yield *zero effect* —
**it breaks the build**. The disqualifying gap is a single field:
`Function.start_line` is never populated by the profiler's OTLP reporter.

Client-facing consequence: we may promise **"eBPF → flame graphs"** only. PGO must
come from **native pprof** (`/debug/pprof/profile`). "PGO from eBPF" is not deliverable
without us adding a symbolization/`start_line`-reconstruction step of our own.

## Live eBPF capture (GCP VM) — the real profiler, real kernel

A live run was done on a GCP VM (Ubuntu 22.04, kernel **6.8-gcp with
`/sys/kernel/btf/vmlinux` present**; VM deleted after). Built `ebpf-profiler` from
source (containerized toolchain), ran it against the same hot Go workload, and
shipped OTLP profiles to a purpose-built sink (uses the agent's own `pprofile`
pdata library so the wire format matches) that reports `Function.start_line` and
per-location inline frames directly.

The eBPF tracer **loaded and attached** on the real kernel. Across all 6 OTLP
exports the result was unambiguous:

```
functions: N total, 0 with start_line>0 (ALL)          # every export
locations: M total, 0 with >1 line (inline frames), max lines/loc=1
sample Go functions:
  - main.process        (.../hot.go)  start_line=0
  - main.(*scaleOp).Apply (.../hot.go) start_line=0
  - main.mix            (.../hot.go)  start_line=0
  - runtime.mallocgc    (runtime/malloc.go) start_line=0   ...
```

So on a real kernel the real profiler:
- **symbolizes correctly** (Go names + `.go` filenames) — req1/req3 OK;
- emits **`start_line=0` for 100% of functions** — req5 FAIL;
- emits **no inline frames at all** (`max lines/loc=1`) — req4 FAIL.

This empirically confirms the source reading below on live data: an eBPF
profile from this agent, converted to pprof, is exactly the `nostart` case and
fails `go build -pgo`.

### Aside — the first environment could not run it

The original Docker Desktop linuxkit 5.15 / arm64 VM has **no kernel BTF** and
`perf_event_paranoid=3`, so the CO-RE profiler could not load there. A GCP VM
(BTF present) was needed for the live run. Separately, the format question was
also settled by (1) a controlled ablation of a real native pprof via Go's own
toolchain, and (2) reading the profiler's OTLP-emitting source — both below.

## Method

- Test binary: a small Go service (Go 1.26.1) with a monomorphic-in-practice
  interface call in a hot loop (`op.Apply` where `op` is always `*scaleOp`) — a
  canonical PGO devirtualization target. Exposes `net/http/pprof`.
- Captured native CPU profile via `curl .../debug/pprof/profile?seconds=20`.
- Built an analyzer/ablator on `github.com/google/pprof/profile` that (a) checks the
  four Go-docs format requirements and (b) writes degraded copies simulating what an
  eBPF converter omits:
  - `nostart`  — `Function.start_line` zeroed (req 5 removed)
  - `noinline` — each Location collapsed to its outermost frame (req 4 removed)
  - `ebpflike` — both removed
- Built/benchmarked the same package five ways: `off`, `native`, and the three
  ablations, capturing compiler PGO decisions (`-gcflags=-m=2`, `-d=pgodebug=1`).

## Results

### Native pprof satisfies all four format requirements
```
sample_type: samples/count cpu/nanoseconds   -> req1 OK
symbolized (Function.name):   44/44          -> req3 OK
inline frames: 8/81 locations have >1 line   -> req4 OK
start_line filled: 44/44 functions           -> req5 OK
```
Compiler acts on it:
```
./hot.go:53:18: PGO devirtualizing interface call op.Apply to (*scaleOp).Apply
pgodebug: hot-node enabled increased budget=2000 for func=main.(*scaleOp).Apply
```
(Absent under `-pgo=off`.) So the native profile *functionally* drives PGO.

### Build matrix

| profile   | start_line | inline frames | builds? | PGO devirtualization |
|-----------|:----------:|:-------------:|:-------:|----------------------|
| off       | —          | —             | yes     | no (baseline)        |
| native    | yes        | yes           | yes     | **yes** (hot.go:53)  |
| noinline  | yes        | no            | yes     | **yes** (still fires)|
| nostart   | **no**     | yes           | **NO**  | build fails          |
| ebpflike  | **no**     | no            | **NO**  | build fails          |

Build failure (both `nostart` and `ebpflike`), verbatim:
```
preprofile: error parsing profile: profile missing Function.start_line data
(Go version of profiled application too old? Go 1.20+ automatically adds this to profiles)
FAIL <pkg> [build failed]
```

Key isolation: **`start_line` is the killer.** Removing it fails the build outright.
Removing inline frames alone does NOT fail the build and, for this interface-call
pattern, PGO devirtualization still fired (because the un-inlined interface call
already leaves `Apply`/`mix` as physical frames). Inline-frame loss is a
quality/iterative-stability degradation, not a hard stop — `start_line` is.

Runtime benchmark delta (native vs off) was within noise here (~8.6 µs/op both) —
the monomorphic indirect call is essentially free on this CPU. This does not weaken
the conclusion: the eBPF-shaped profile never reaches runtime — it fails at build.

## Confirmation against the real profiler source

`go.opentelemetry.io/ebpf-profiler@v0.0.202632`, OTLP reporter
(`reporter/internal/pdata/generate.go`):

- FunctionTable population sets **only** name and filename — no start_line:
  ```
  f.SetNameStrindex(v.nameIdx)
  f.SetFilenameStrindex(v.fileNameIdx)   // no SetStartLine anywhere in reporter
  ```
- The only `start_line` code in the whole module is inside the **V8 (Node)** and
  **HotSpot (Java)** interpreters for their internal function-offset math — never in
  the native/Go path and never in OTLP output.
- Sample type is `samples`/`count` (req1 OK); each frame becomes one Location with a
  single Line carrying the source line (req3 OK).

So a real eBPF profile, converted OTLP→pprof for PGO, is exactly the `nostart` case
→ Go build failure. (Separately: the profiler emits **OTLP profiles**, not pprof;
feeding Go PGO needs an OTLP→pprof step, which cannot invent a `start_line` that was
never captured.)

## Implications for ADR 0011

1. **Promise wording.** "eBPF → flame graphs, PGO only from pprof." Do not claim
   "PGO from eBPF" for the vanilla upstream profiler.
2. The hypothesis is confirmed and sharpened: it is not "great flame graph, zero PGO
   effect" — current Go makes it a **hard build failure**, which is louder and better
   than a silent no-op, but still a no-go.
3. If we ever want real "PGO from eBPF", the agent must reconstruct `start_line`
   (and ideally inline frames) during symbolization from the Go binary's pclntab —
   i.e. our own symbolizer, not upstream output. This is where an in-house symbolizer
   would earn its keep, now with a concrete required field: `Function.start_line`.
4. Net for the product: the pprof-pull path is the *only* PGO source today. The eBPF
   DaemonSet stays justified for coverage/flame graphs, not PGO.

## Decision & scope

**PGO is deferred.** Not this phase — neither variant:

### Deferred (do later)
- **PGO delivery from native pprof** (auto-merge `default.pgo`, hand it back / PR it).
  Technically ready (all 4 requirements met, effect confirmed) but out of scope now.
- **PGO from eBPF** — blocked outright: no `Function.start_line` → `go build -pgo`
  fails. Requires our own symbolizer to reconstruct `start_line` (+ inline frames)
  from the binary's `pclntab`. The OTLP model already has the field
  (`pprofile.Function.SetStartLine`); it is a localized symbolization change, not a
  protocol gap. Size to be estimated when we pick PGO back up.
- Revisit trigger: once the profiling/attribution work is shipping and there is
  demand for build-time speedups.

### Taken now (the useful part, no PGO)
The value that does NOT depend on PGO and ships independently:
- **Hot-function attribution / flame graphs** from the eBPF profiler on the node
  (ADR 0011), symbolized and allow-list-filtered on the node.
- **Go + pprof detection** and profile storage keyed to image digest.
- eBPF is flame-graphs/coverage only, never PGO, and only on BTF-capable kernels.

Net: we collect and attribute "which function burns resources" now; we do not emit
any PGO artifact yet.

## Reproduce

Method, no local paths:

1. Write a small Go service with a monomorphic interface call in a hot loop and a
   `net/http/pprof` endpoint; capture `.../debug/pprof/profile?seconds=20`.
2. Ablate the captured pprof with `github.com/google/pprof/profile`: zero every
   `Function.start_line` (`nostart`), collapse each Location to its outermost line
   (`noinline`), and both (`ebpflike`).
3. Build the same package with `-pgo=<profile>` for each variant and observe:
   ```
   go build -a -pgo=<profile> -gcflags=-m=2 . 2>&1 | grep 'PGO devirtualiz'
   ```
   `nostart`/`ebpflike` fail at `preprofile` (missing `start_line`); `native`/`noinline`
   build and devirtualize.
4. For the live check: build `go.opentelemetry.io/ebpf-profiler` on a BTF kernel
   (5.8+), run it against the workload, and inspect the emitted `Function.start_line`
   via an OTLP-profiles sink built on `go.opentelemetry.io/collector/pdata/pprofile`.
