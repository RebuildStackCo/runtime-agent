# 0077. A restart resumes every open window, not only the usage one

Date: 2026-09-10
Status: Accepted
Amends: 0072 §1, 0072 §2

Widens the startup read of ADR 0072 from one kind to five: the four journal
windows — `container_restarts`, `pod_disruptions`, `node_lifecycle`, `job_runs` —
resume the same way `usage_snapshot` does. Fixes silent record loss, not a
gap: a restart inside an open window could write a subset of that window over
the complete one. Changes no payload, no natural key, no cadence and nothing
about what is collected.

## Context

ADR 0072 gave the agent a startup read of its own spool so a restart resumes the
hour it was in the middle of. It read `usage_snapshot` and nothing else. The
journals were not argued against; they were not considered.

Nothing in the four journal accumulators survives a restart — each is a map in a
struct, rebuilt empty by `NewRestarts` and its three siblings. The collector that
feeds them rebaselines on first sight of a container and deliberately emits
nothing for it, which is correct on its own terms: re-emitting a counter the
previous process already counted would double it.

The two facts meet in the flush. `cmd/agent` writes one file per window from
`append(CloseBefore(now), Snapshots()...)`, and `Spool.WriteContainerRestarts`
groups those records by window and renames a file over each one. So:

- If nothing is observed for a window after the restart, that window contributes
  no records, no group and no write. The file survives untouched, which is why
  this was never noticed.
- If **anything** is observed for that same still-open window — one container
  restarting at minute 40 of an hour whose file already held three — the flush
  writes a payload holding only the new record, over a file holding all of them.

Measured against the real `Spool`: a window file holding two records and fourteen
restarts became one record and one restart.

What makes it a contract violation rather than an internal quirk is the other
side. `backend-requirements.md` §4 tells the backend to upsert by (window start,
window length) and never to add successive deliveries together, and §6 states
that a superseding payload is the complete state under its key. After a restart
it is not, and the payload says nothing to distinguish the two — so the backend
does exactly what it was told and replaces a complete window with a subset.
Since ADR 0075 the payloads travel, so this is loss at the far end and not only
on disk.

Three properties make the fix cheaper here than it was for usage:

- **Double counting is already prevented.** The collector's first-sight
  rebaseline means resumed records are added to, never counted again. ADR 0072 §5
  needed a per-key guard for usage; the journals need none.
- **The end-of-window rule is simpler.** A journal window has one filename
  whether it is open or final, so there is no closed record to look for: if the
  window has ended, the file *is* the final record and is left alone. ADR 0072 §4
  needed two checks; this needs one.
- **Validation is smaller.** Kind, window start, window length, and every record
  belonging to the window its file names. No histogram grid, no frozen buckets.

## Decision

**1. The startup read covers five kinds.** Each of the four journal windows is
read on the same terms ADR 0072 set for `usage_snapshot`, and its records seed
the accumulator through a new `Resume`. A window that has already ended is not
reopened. A file of foreign or damaged shape — wrong kind, wrong window, records
belonging to another window, past the 64 MiB ceiling — is skipped, counted and
left exactly where it is.

**2. `Resume` never overwrites an observation.** A key already present in an
accumulator is left alone, so a record written by a previous process cannot
displace one this process saw. In practice the map is empty when this runs; the
rule is stated because it is what makes the ordering of startup not matter.

**3. The counts stay apart from the usage ones.** `spool_journal_windows_resumed`
and `spool_journal_records_resumed` join the ADR 0072 gauges rather than being
folded into them: they answer about different accumulators, and a summed number
would hide four kinds resuming nothing behind one kind resuming a lot.

**4. What a restart still costs is unchanged.** ADR 0072 §7 priced it and every
item stands: up to one flush interval of observations that were never written,
and — because the spool is an `emptyDir` by decision (ADR 0007, ADR 0026) — the
whole of it when the pod itself is replaced rather than the process. This
decision recovers what reached the disk. It does not make the disk durable, and
nothing here should be read as claiming a rescheduled agent loses less than it
did.

## Consequences

**Easier.** A controller restart stops being able to destroy data that was
already correct on disk and already delivered. The backend's instruction to
treat a superseding payload as complete becomes true for these four kinds
instead of nearly true, which is the difference between a contract and a
convention.

The reading of the spool is now one shape rather than a special case:
`readRecoverable` bounds any file the startup read will decode, and the journal
window envelope is validated by one generic function over four record types.
Adding a fifth window kind is a case in a switch, not a new mechanism — and the
filename prefixes are now constants shared by the writer and the reader, so the
two cannot drift the way ADR 0073 §5 describes.

**Harder / given up.** The spool is read more at startup: five kinds of file
rather than one, on a directory whose size is already bounded (ADR 0042). The
read stays a pure one — nothing is deleted, rewritten or renamed on the strength
of anything it finds — so the failure mode of the whole mechanism is still the
behaviour that preceded it.

`security.md`'s promise that the agent reads exactly one kind of its own file is
no longer true and is restated as five, named. That promise was worth something,
and what replaces it has to carry the same weight: the list is closed, it is in
the document, and a sixth kind means changing it there.

**Not changed.** Any payload, any natural key, any cadence, what is collected,
what is filtered, when anything is sent, or the one-way protocol. No golden byte
moves.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
