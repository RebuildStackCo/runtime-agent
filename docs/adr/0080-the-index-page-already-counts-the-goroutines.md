# 0080. The index page already counts the goroutines

Date: 2026-09-11
Status: Accepted

Adds the payload kind `goroutine_counts`: a series of goroutine counts per
sampled process per window, read off the `/debug/pprof` index page the agent
already fetches. Adds no new path, no new privilege and no new annotation. It
rides on `profiling.pprof.enabled` rather than on `profiling.pprof.pull`, and
it deliberately collects no stacks.

## Context

A goroutine leak is one of the findings a workload report is expected to carry,
and the agent had no sensor for it at all. The obvious sensor is
`/debug/pprof/goroutine`, and the obvious shape is the one `pprof_profile`
already has: pull it on a rotation, filter it, ship it (ADR 0058).

Measuring that first is what changed the design.

**Serving a goroutine profile is the most expensive thing the agent could ask
of a workload, and the expense is memory rather than latency.** The runtime
stops the world twice, both times briefly and with almost no work inside — the
walk over all goroutines runs with the world going. But the collector allocates
a record per goroutine before it can write one. Measured on Go 1.26 against a
process parked at a million goroutines:

| | index page | goroutine profile |
|---|---:|---:|
| wall | 134 µs | 3.92 s |
| CPU | 117 µs | 7.90 s |
| **heap allocated in the workload** | **0** | **726 MiB** |
| stop-the-world pauses | 0 | 2, ~87 µs total |
| response | 2 604 B | 2 489 B |

The allocation is linear in goroutines and in stack depth: ~0.8 KiB per
goroutine at depth 4, ~1.4 KiB at depth 8, ~3.1 KiB at depth 32. So a pull from
a workload near its memory limit can be the thing that ends it, and the workload
most likely to be near it is the one that is leaking. Nothing else the agent
does can kill what it measures.

Two further facts about that path, both measured, both recorded here so a later
slice does not rediscover them: a client-side timeout protects nothing, because
the collection finishes inside the target before a byte is written; and
`writeRuntimeProfile`'s retry loop, which re-runs the whole collection when the
count grew, never fired at 20 000 new goroutines a second and did fire at
~450 000 — 43 pauses, 7.4 s, 2.4 GiB for one request.

**The count on its own costs nothing, and it is already on a page the agent
fetches.** `net/http/pprof`'s index renders one row per profile with that
profile's count, and for `goroutine` the count is `runtime.NumGoroutine()` — a
variable read. The prober already GETs that page to confirm an endpoint
(ADR 0057), and reads only its title.

**And a leak is a trend, not a value.** No single reading is a finding: 4 000
goroutines is a leak or a healthy concurrency level depending entirely on what
the number did over the preceding hours. Stacks answer *where*, and only once
*whether* has been answered. So the cheap reading is not a degraded version of
the profile — it is the part that carries the finding, and the profile is the
follow-up.

What the count cannot do is say where. That is left to a later slice, on the
terms recorded in [`plans/goroutine-leak-sensor.md`](../plans/goroutine-leak-sensor.md).

## Decision

**1. The count is read off the index page, and no new path is requested.** The
same `GET /debug/pprof/` the prober makes, repeated. `/debug/pprof/goroutine` is
not fetched, in any `debug` mode; `debug=2` in particular dumps every goroutine
individually with its function arguments, and is in the class of
`/debug/pprof/cmdline` — a path this agent's code may not name (ADR 0057).

The title is checked on every reading and not only on the first. The prober's
answer is about a build; what answers on that address today is a separate
question, and digits scraped out of a stranger's page would be a series about
nothing.

**2. Every confirmed endpoint, every minute.** Not a rotation. The pull is
round-robin because each profile stands alone (ADR 0058 §2); a series does not,
and a workload sampled half the time yields a series with holes exactly where
the interesting part might be. One reading per process per round — a container
binding three ports is one runtime with one count.

A minute because that is the flush interval: sampled more finely, the series
holds readings a replaced pod may never spool; more coarsely, every window has
gaps in it. The round is bounded by its own interval so two rounds never
overlap, and by 200 processes so a very large cluster does not spend a round on
one namespace.

Those two bounds are what makes a third necessary. A target that fails costs the
round the whole connect timeout, and the order is deterministic, so a handful of
dead endpoints near the front would spend every round and the tail would never
be reached — "every endpoint" true only of the first few. So a failed reading
rests its target for five minutes, the idiom the pull path already uses for a
refusal (ADR 0058 §3): a broken endpoint costs one reading in five instead of
every one, and a pod that has come back is sampled again inside the same window.
A success clears the rest.

**3. The sample names the pod, and this is the one place the funnel does.**
Everything else in the pprof funnel is keyed by build, because every replica of
a build answers a question about the build identically (ADR 0057 §3). A
goroutine count is not that kind of fact — it belongs to one process. A series
that could not name its pod would read a pod replacement, where the new process
starts near zero, as a leak that cured itself. The pod name is already collected
by the restart and disruption journals; the address still is not.

**4. It is a window of readings, shaped like a journal and classed as
`measured`.** Same envelope as the four journal windows — `{kind, source,
window_start, window_seconds, captured_at, records}` — superseding within its
window, one file per window, resumed at startup by the machinery ADR 0077 built,
final when `captured_at` is at or after the window's end (ADR 0078). ADR 0077's
consequences predicted a fifth kind would be a case in a switch; it was.

The provenance is `measured` and not `journal`: the facts are readings the agent
took from an instrument, not history derived from object status. That widens
`measured`'s own definition from the kubelet to any instrument the agent polls,
which is what it always meant. The window shape is about delivery, not about
what class of claim a sample is.

**5. It rides on `pprof.enabled`, not on `pprof.pull`.** `pull` is the switch
for asking a process to *produce* something — it runs that process's CPU
profiler for ten seconds and needs the single slot Go allows. This produces
nothing: it is the request `enabled` already describes, repeated. A third switch
was considered and rejected as a control surface bought for nothing.

What does change under `enabled` is frequency: from once per image and port,
ever, to once per process per minute. That is a real widening of a promise and
`security.md` states it rather than folding it in. The cost is below what the
cluster already spends probing the same pod — the chart's own containers are
probed every ten seconds.

**6. Every outcome is counted, and the two absences are separated.**
`unreachable` is a target with no addressable replica or a connection that
failed, which says nothing about the endpoint. `unreadable` is an answer that
arrived and carried no count. A workload with no series and no reason is the
silence ADR 0054 exists to end.

**7. A record is a series, and the payload says when it is thinner than it
looks.** Readings past the per-window cap are dropped and counted in
`samples_dropped`, so a reader can tell a gap in the series from a gap in the
process.

## Consequences

**Easier.** The agent finally collects the fact a goroutine leak is made of, at
a cost that does not have to be argued: zero allocations and zero pauses in the
workload, and a payload whose size does not depend on how many goroutines there
are. A cluster of two hundred endpoints adds about a megabyte an hour before
compression.

Because the count is a series, it joins the facts the agent already collects
into a story none of them tells alone: a count climbing for six hours and then
an `oom_kill` under the same workload key is a diagnosis, not a coincidence.

**Harder / given up.** The agent now connects to a workload repeatedly rather
than once. Small — a 134 µs read of a page rendered from memory — but it is a
recurring request where there was a single one, and the customer learns it from
`security.md` rather than from a packet capture.

The finding stops at *whether*. The agent reports a series and names no leak;
without the follow-up slice, a customer is told their goroutine count is
climbing and not which code is responsible. That is a real gap and it is the
honest state of this slice.

Only one replica of each workload is read, chosen stably. A leak confined to a
replica that is not the chosen one is invisible until that pod is replaced.
Reading every replica was rejected: it multiplies the connections by the replica
count to catch a case that a leak, by its nature, does not stay in for long.

Nothing is collected where there is no node DaemonSet, since the whole funnel
stands on facts read from the binary (ADR 0056).

**Not changed.** What is filtered, any existing payload, any natural key, any
cadence, the one-way protocol, the annotation that excludes a workload from
every profiling path, or what `pprof.pull` does. No golden byte of an existing
kind moves.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
