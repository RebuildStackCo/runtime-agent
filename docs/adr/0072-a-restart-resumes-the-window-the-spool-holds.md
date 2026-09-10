# 0072. A restart resumes the open window the spool already holds

Date: 2026-09-07
Status: Accepted
Amends: 0003, 0007, 0013 §2, 0026
Amended by: 0077

Makes the spool readable by the agent that wrote it, which ADR 0003's layout
allowed and no code had ever done. Prices the restart that ADR 0007 and ADR 0026
both reasoned about without naming, and corrects what ADR 0013 §2 claims
`CoveredNanoseconds` shows across one.

## Context

A controller restart is not a rare event. It is the ordinary end of an OOM kill,
an eviction, a node drain, a rolling upgrade and every change to a Helm value.
What it costs was never measured, and the measurement is worse than the estimate.

Usage windows are UTC-aligned and an hour long (ADR 0006). The accumulator
holding the open ones is a map in the process and nothing else. An open window's
snapshot is written under a filename built from the window key —
`usage-<start>-<length>.snapshot.json` — and `Spool.write` renames a temp file
over it, which is exactly the supersede-by-key discipline the backend implements
(ADR 0027).

Put together, those three facts cost a window:

1. The new process opens the same window from an empty accumulator, because the
   window is a function of the wall clock and nothing else.
2. Its first snapshot, a minute later, lands on the filename holding up to
   fifty-nine minutes of the previous process's observations and replaces them.
3. When the window closes, the `usage_window` record is built from the
   accumulator, so the final record of that hour carries only the part after the
   restart.

Two things measured while confirming this were worse than the claim.

**Coverage does not make the loss visible. It hides it.** The first observation
of a container attributes the whole cumulative counter back to container start
(`ingestContainer`), and `CoveredNanoseconds` is the same interval split the same
way. So a process that restarts forty minutes into an hour and takes one more
poll reports fifty minutes of coverage for forty minutes of observation. The
field ADR 0013 §2 introduced to answer "how much of this window did we see?"
answers with more than was seen. Measured: 3000 s claimed against 2400 s
observed.

**The same attribution rewrites windows that were already final.** Smearing the
counter from container start reaches back through every hour the container has
lived. Those windows are closed the moment the first flush runs, and
`WriteClosedWindows` writes them under the key the correct records already
occupy. Measured on a container idle before the window and busy inside it: the
09:00 window went from 0 core-nanoseconds to 90 000 000 000, CPU burned after
10:00 attributed to an hour that had been finished and shipped.

**The material to fix it is already on disk and already provably mergeable.**
The snapshot in the spool is at most one snapshot interval old, it is written in
the bytes the schema is defined by, and rollup records merge associatively and
commutatively by property test. Resuming a window from its own snapshot is that
property applied to the agent's own history — the case the property was
introduced for.

**Configuration is a different question and gets a different answer.** The
neighbouring idea — rereading the ConfigMap so a filter change does not need a
restart — is refused. A window assembled half under one filter set and half under
another is a window whose coverage report has to express which half, and that
complexity lands in the one payload whose job is to be trusted about the agent
itself. Filters are read once at start (ADR 0003), and a filter change is a
restart, which is now cheaper than it was.

## Decision

**1. Before anything else runs, the agent seeds its accumulator from its own
spool.** The startup read takes every `usage_snapshot` file whose window has not
ended, and merges its records into the open windows they name. An empty, absent
or unreadable spool seeds nothing and leaves the agent exactly where it was
before this decision: collecting, from nothing.

**2. The spool is no longer write-only, and this is the change, not a detail.**
Until now the agent only ever wrote payload files and listed the directory to
sweep it. It now reads one kind of file, once, at startup. Three properties bound
what that means:

- **It is a pure read.** No file is deleted, rewritten or renamed on the strength
  of anything the read found. A payload this process cannot decode is still a
  payload the backend can ingest, and destroying it to tidy up would turn a
  parsing problem into data loss.
- **The input is the agent's own prior output**, not anything a backend sent. The
  one-way protocol of ADR 0001 is untouched: nothing outside the cluster can
  reach these bytes, and nothing in them changes agent behaviour beyond the
  contents of a window this agent measured.
- **It reads one kind and nothing else.** Snapshots of open windows, recognised
  by the filename the writer builds and re-checked against the payload's own
  `kind`, `window_start` and `window_seconds`, with every record confirmed to
  belong to the window its file names.

**3. A file of foreign or damaged shape is skipped, counted and left alone.**
Truncated by a crash, holding another kind, naming a window other than the one
inside it, carrying a histogram bucket that is not on the frozen grid, or larger
than a 64 MiB ceiling — each is one entry in the startup log and one increment of
`spool_recovery_files_skipped`, and the file stays where it is. Nothing here is
fatal; the failure mode of the whole mechanism is the behaviour that preceded it.

**4. A window that has already ended is not reopened, and neither is one that
already has a closed record.** The first would put an accumulator back into an
hour the cluster has left; the second would let a later close supersede a final
record with a subset of itself. Both leave the file exactly as it is: it ships as
a snapshot and expires with the sweep.

**5. A resumed key's first observation only rebaselines.** The rule that
attributes a container's whole counter from its start exists to fill a gap the
agent did not watch. Where this process resumed that key's window, there is no
such gap — the observations are in the record — and applying the rule would add
them twice and re-smear them across closed windows. So for a resumed key the
first observation after the restart establishes the baseline and emits nothing,
for CPU, both PSI counters and CFS throttling alike. The guard is per key and
lifts once the window it protects has ended, so a container the spool said
nothing about is still attributed from its start.

**6. Nothing about the payloads changes.** No field is added, no ordering field
appears (ADR 0027 stands), no golden byte moves, and the backend contract is
unchanged. Recovery is visible from inside the cluster only, on the metrics
endpoint of ADR 0070: `spool_windows_resumed`, `spool_records_resumed` and
`spool_recovery_files_skipped`, gauges because the read happens once.

**7. The price of a restart, in full.** Named here because it was never named,
and because every item is now the whole of it:

- **Up to one snapshot interval (60 s) of observations** accumulated after the
  last snapshot and never written. Unrecoverable by construction: they exist only
  in the process that had them.
- **One poll interval (30 s) per already-observed key**, being the first counter
  difference the new process cannot compute. This is the cost of decision 5 and
  the reason it is cheap: the alternative is double-counting an hour.
- **The whole open network window.** `network_window` has no snapshot sibling by
  the deliberate choice of ADR 0053 §4 — a partial window of accumulated bytes
  read as a rate over the full window is simply wrong — so the spool holds
  nothing to resume, and up to an hour of pod network counters is lost. Fixing
  this means inventing a snapshot kind for a payload that refuses one; it is not
  done here and is not a defect of this decision.
- **Everything the spool no longer holds.** A pod rescheduled onto another node
  gets an empty `emptyDir` (ADR 0026) and recovers nothing, which is the case
  ADR 0007 already analysed. There the counter smearing of decision 5's exception
  is right rather than wrong: it fills a gap nobody observed.
- **In-memory collector state that is not the accumulator** — counter baselines,
  the signal set, profiling rotation — is not recovered and is not made
  recoverable. It is rebuilt from the cluster within one poll, which is what
  CLAUDE.md invariant 5 asks of it.

## Consequences

**Easier.** A restart during a window costs a minute instead of the rest of the
hour, and the number the backend receives for that hour is the number the cluster
produced. Every restart class benefits equally, and the one that used to be
argued about — changing a Helm value — is no longer the expensive one. Closed
windows stop being rewritten behind the agent's back, which was silent and is now
tested.

**Harder / given up.** The spool has a reader, so it has a decoder, so the
payload bytes are now a format the agent must be able to consume as well as
produce. `rollup.Record` gained `UnmarshalJSON` and the histogram gained the
inverse of its frozen marshalling; a round-trip property test stands next to the
merge property tests because a lossy decode here would be a wrong number rather
than an error. The startup path grew a step that touches the filesystem before
the agent has done anything, and it is written so that every way it can fail ends
in the old behaviour.

The first counter difference after a restart is now missing for keys that were
resumed, where before it was present and wrong. That is a deliberate trade of an
overstatement for an understatement, and `CoveredNanoseconds` says which by
falling rather than rising.

**Not changed.** What is collected, filtered or transmitted. No payload field,
no golden byte, no backend obligation, no configuration knob. There is still no
embedded database and no persistent state that the cluster or the backend cannot
rebuild: what the spool now supplies at startup is a convenience whose absence
costs exactly what it cost yesterday.

**Deliberately not done.** Hot configuration reload, for the reason in Context.
A snapshot kind for network windows. Any recovery of the counter baselines, which
would be persistent state that nothing outside the process can rebuild — the one
thing invariant 5 forbids outright.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
