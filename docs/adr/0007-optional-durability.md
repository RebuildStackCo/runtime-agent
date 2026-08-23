# 0007. Durability is optional: snapshot delivery bounds loss, disk is a knob

Date: 2026-08-09

Status: Accepted
Amends: 0003, 0006
Amended by: 0008, 0026

Amends 0006's delivery consequences; refines 0003's volume assumption — the
spool format and no-database rules of 0003 stand unchanged.

## Context

ADR 0003 assumed a PersistentVolume as the home of identity, checkpoint, and
spool, and the backend contract promised that "the PVC holds days of
rollups". Both predate ADR 0006, whose minute-cadence snapshot delivery with
supersede-by-key ingest changed the loss math:

- In normal operation, everything up to the last minute is already on the
  backend. An agent restart loses at most one snapshot interval.
- During a backend outage, open and closed windows accumulate in memory —
  megabytes per day, not a storage problem.
- Disk changes the outcome in exactly one scenario: an **agent restart while
  unacknowledged data exists** (i.e. during a backend outage). Without disk
  that span's distributions are gone and its counter totals are smeared
  uniformly across the gap by the first-observation rule; with disk it is
  preserved.

Mandating a PVC prices that one scenario into every installation: a
StorageClass requirement, provisioning permissions, and an operational
dependency some enterprise clusters flatly disallow. Meanwhile the demand it
served is mostly gone — freshness comes from snapshots, not from disk.

## Decision

1. **Disk is a durability knob, not a requirement.** The default volume is
   an `emptyDir`: it survives container restarts on the same node (the most
   common restart class) and vanishes on rescheduling. Customers who want
   unacknowledged data to survive rescheduling and node loss enable the PVC
   (`persistence.enabled: true`); nothing else about the agent changes.
2. **The contract stops promising days of disk.** The backend must tolerate
   an agent that reconnects after an outage with anything from a full spool
   backlog to nothing but fresh data. Routine loss is bounded by the
   snapshot cadence (~1 minute); worst-case loss is the outage-and-restart
   span, and the coverage report makes any gap visible rather than silent.
3. **The spool format of ADR 0003 is unchanged** — payload files named by
   natural keys, deletion as acknowledgment, a checkpoint file, no embedded
   database. What changed is only where that directory may live and how
   long it is promised to survive.
4. **Retention is time-bounded, not size-unbounded**: spool files older than
   a configured maximum age are deleted even without acknowledgment, so an
   extended outage degrades to the memory-only behavior instead of filling
   the volume.

## Consequences

- Installation loses its only storage prerequisite; `metrics-only` runs on
  any cluster with no StorageClass at all.
- A pod rescheduled during a backend outage loses the unacknowledged span —
  accepted, bounded, and visible in coverage. Enterprises that cannot accept
  it have a one-line opt-in.
- Identity (key, certificate) on `emptyDir` means rescheduling triggers
  re-enrollment; the token-TTL and re-enroll flow of ADR 0005 already cover
  this, and the PVC opt-in removes it for those who care.
- The backend cannot assume agents re-supply history after long outages
  (`backend-requirements.md` §4 and §8 already forbid relying on that).
