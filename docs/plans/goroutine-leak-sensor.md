# Development plan — the goroutine leak sensor

Living plan for the two slices that make a goroutine leak visible. Not
normative like an ADR: it records *how* we build, slice by slice. When a slice
ships, tick it and correct this file to match what actually landed.

- Design authority: [ADR 0080](../adr/0080-the-index-page-already-counts-the-goroutines.md)
  for slice A. Slice B needs its own ADR, written with it.
- Customer contract touched: [`security.md`](../security.md) §8 (the payload
  table) and §10.3 (what the agent asks of a workload).
- The funnel this rests on: [ADR 0056](../adr/0056-a-pprof-endpoint-is-proved-not-probed.md)
  (proof from the binary), [ADR 0057](../adr/0057-the-controller-confirms-an-endpoint-once.md)
  (confirmation), [ADR 0058](../adr/0058-the-pull-starts-a-profiler-and-the-binary-says-whose-code-it-is.md)
  (the pull and what it costs).

## The split, and why it is a split

A leak is a trend. **Whether** a process is accumulating goroutines is a
question about a series of counts; **where** is a question about stacks. The
two cost different amounts by four orders of magnitude, so they are two slices
and not one — measured on Go 1.26 against a process at 10⁶ goroutines:

| | index page (A) | goroutine profile (B) |
|---|---:|---:|
| CPU in the workload | 117 µs | 7.90 s |
| heap allocated in the workload | 0 | 726 MiB |
| answers | whether | where |

Slice A carries the finding. Slice B makes it actionable.

## Slice A — the count (shipped)

- [x] `goroutine_counts`: a series per sampled process per window, read off the
      `/debug/pprof` index page the prober already fetches.
- [x] Read every confirmed endpoint every minute, one reading per process.
- [x] Fifth journal-shaped window kind: supersedes within its window, resumes
      at startup (ADR 0077), states finality with `captured_at` (ADR 0078).
- [x] Coverage block and `pprof_count_readings_total`, with `unreachable` and
      `unreadable` separated.
- [x] Contract, promise and chart values.

**Not done, and worth doing before B.** The e2e suite confirms a `pprof_profile`
for its Go sample but asserts nothing about `goroutine_counts`, though the
sample serves the index page the reading comes from. One wait and one assertion
in `test/e2e/inventory_test.go`.

## Slice B — the stacks (not scheduled)

Pull `/debug/pprof/goroutine?debug=0` from a process whose series says it is
accumulating, and ship the filtered stacks so a report can name the code.

**The preconditions are the whole of the slice.** None of them is optional.

1. **A memory-headroom gate, and it is not decoration.** ~1.4 KiB of transient
   heap per goroutine, inside the customer's process. A pull from a workload
   near its limit can be the thing that OOM-kills it, and the workload most
   likely to be near its limit is the one that is leaking. The controller
   already holds what the gate needs: `MemoryLimitBytes` in
   `internal/collector/pods.go`, used the same way by `internal/collector/oom.go`.
2. **Only the first pull of a target is blind.** A goroutine profile reports its
   own count, so every pull after the first is priced exactly. The first needs
   an absolute headroom floor.
3. **A client timeout protects nothing.** The collection finishes inside the
   target before a byte is written, so a deadline on our side bounds our wait
   and not their cost. Any protection has to be a decision not to ask.
4. **A separate validator.** `internal/nodeprofile/validate.go` requires a
   `cpu`/`nanoseconds` sample type; a goroutine profile is `goroutine`/`count`.
   Widen it into a second function rather than a second accepted type — two
   profiles promise the customer different things, and one validator taking both
   is where they get confused for each other.
5. **Its own kind**, on the precedent that `ebpf_profile` and `pprof_profile`
   are separate kinds purely because the capture differs (ADR 0012 §2). Here the
   measured quantity differs too: a count, not nanoseconds.
6. **`debug=2` is forbidden**, in code and in the ADR. It dumps every goroutine
   individually with its function arguments, which puts it in the class of
   `/debug/pprof/cmdline` (ADR 0057).
7. **Triggering contradicts ADR 0058 §2 unless the argument is made.** That
   decision rejected ranking targets by consumption, because the finding a
   report needs is usually about a workload nobody suspected. The answer here:
   slice A covers every endpoint evenly, so what is ranked is only the expensive
   follow-up, and the trigger is the phenomenon itself rather than a proxy for
   it. The ADR has to say this, not assume it.
8. **`security.md` §10.3 gains a third path** with a cost unlike the other two,
   and the switch question has to be settled: `pprof.pull` covers it, on the
   reasoning that `pull` is the switch for asking a process to *produce*
   something. Holds should then become per (target, path), so a workload whose
   own profiler holds the CPU slot still yields goroutine stacks.

**What would say B is worth building:** installed agents reporting series that
actually climb. Until then A is the sensor and B is a plan.

## Known limits of both slices

- Only one replica of each workload is read. A leak confined to another replica
  is invisible until that pod is chosen.
- A stack lying entirely in third-party code is redacted by default
  (`thirdPartySymbols: "drop"`), so slice B can report that something leaks
  without naming it. The standard library and the runtime survive the filter,
  which covers most classic leak stacks but not all.
- Nothing here works without a node DaemonSet: the funnel stands on facts read
  from the binary.
- `GOEXPERIMENT=goroutineleakprofile` adds a first-class
  `/debug/pprof/goroutineleak` upstream. Off by default in Go 1.26, so no
  customer build has it; if it becomes the default, `GOEXPERIMENT` fits the
  criterion of `buildSettingsAllowList` in `internal/nodescan/scan.go` and the
  agent could know which builds serve it without asking.
