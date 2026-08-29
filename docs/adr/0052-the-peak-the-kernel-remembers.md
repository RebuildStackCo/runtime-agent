# 0052. The peak the kernel remembers, and the second file the scanner opens

Date: 2026-08-29

Status: Accepted

Amends: 0009, 0006 §5

Adds the payload kind `process_peaks`. Widens the node scanner's reads from
`/proc/<pid>/exe` and `/proc/<pid>/cgroup` to include two allow-listed fields of
`/proc/<pid>/status`. No new capability, no new RBAC.

## Context

[ADR 0006](0006-usage-collection-kubelet-rollups.md) named this gap and accepted
it: "the true memory peak between two samples is unobservable", so "peak risk
must come from other signals (OOM events, PSI)". Memory is a gauge, the agent
samples it on an interval, and what happens between two samples is not recorded
anywhere the kubelet exposes. The consequence is that the agent can say a
container was killed for memory, and can say what its working set looked like at
the moments it was asked, and cannot say what it actually reached.

The kernel records it. `VmHWM` in `/proc/<pid>/status` is the high-water mark of
a process's resident memory since it started, maintained continuously rather
than sampled. It is one line of a file the node role is already positioned to
read: the scanner has `hostPID`, runs as uid 0, and walks these directories
every scan.

Two things make now the moment. [ADR 0047](0047-runtime-knobs-are-named-and-kept.md)
ships whether `GOMEMLIMIT` is set, which is a finding nobody acts on without
knowing how close the container runs to its limit. And
[ADR 0050](0050-godebug-defaults-are-a-build-fact.md) removed the reason to read
`Cpus_allowed_list` for its headline finding, which left the question of whether
to open this file at all — decided here, in one place, rather than twice.

## Decision

**1. Two fields, by allow-list, and the promise in `security.md` changes.**
`VmHWM` and `Cpus_allowed_list`. `security.md` §7.1 has said "what it reads is
`/proc/<pid>/exe` and `/proc/<pid>/cgroup` and nothing else"; that sentence is
now a list of three files, and the fields of the third are enumerated.

The keys are an allow-list for a specific reason rather than by habit:
`/proc/<pid>/status` opens with `Name`, the executable's own basename, which is
an identity the agent does not collect. A deny-list would have to keep pace with
a format the kernel owns (ADR 0019's argument, in a file where the stake is
higher).

The read costs no privilege. The kernel's ptrace check guards `exe`, `mem`,
`environ` and `maps`; `status` is not among them, so this needs nothing beyond
what ADR 0009 already granted. It happens after the scope and infrastructure
filters, so a process the agent does not collect has its status file left
unopened (invariant 4).

**2. `Cpus_allowed_list` ships as a count, and its headline finding is gone.**
What leaves is how many CPUs the mask permits, never which ones — which cores a
container sits on says where its neighbours are, and no finding needs it.

The finding this field was originally wanted for — `GOMAXPROCS` sized from the
node rather than the quota — is answered by ADR 0050 from the build, for every
binary on a current toolchain. What remains is narrower and worth one field:
CFS quota does not change the affinity mask, so a count below the node's CPUs
means something pinned the container, and the range across replicas says whether
they are on machines of one size.

**3. A peak belongs to the build that reached it, and to the processes still
holding it.** Two rules, and both exist to stop a number outliving its evidence:

The record is keyed by `(namespace, workload, container, image digest)`, the
extension `workload_metadata` already uses. A deploy that fixes a leak gets a new
digest and a new record rather than inheriting the old one's number.

The store keeps each node's latest contribution and merges them, rather than
accumulating a maximum. A node's contribution is replaced wholesale by its next
report, dropped when the node leaves, and dropped with the workload on the
signal ADR 0018 already defines. So a peak survives exactly as long as a process
somewhere still stands behind it. Accumulating instead would be simpler and
would make `processes` count scans, and would keep a peak whose pod died months
ago.

**4. Its own payload kind, `measured`, superseding, unwindowed.** It cannot ride
in `go_inventory`: that payload is `structural`, and ADR 0012 §2 forbids merging
provenance classes under one key. It is not a window either — a high-water mark
since process start belongs to no interval — so it takes the shape
[ADR 0034](0034-restart-counter-readings.md) settled for the restart counter: a
reading, keyed per cluster, superseding.

A consequence worth naming: one node report now carries facts of two provenance
classes, and the controller splits them into two payloads. Provenance is a
property of a payload, not of a channel, but the channel is where this would rot
unnoticed.

**5. The evidence is one-sided, and the contract says so.** `VmHWM` is the RSS
of one process; the limit is enforced on the cgroup, which also holds page
cache, every other process in the container, and kernel structures. The number
is therefore a *floor* under the container's peak, never the figure the OOM
killer compares against.

That asymmetry is the whole of how it may be read. A peak near the limit is
strong evidence — one process alone nearly fills the cgroup, and everything else
only adds. A peak far below the limit is weak evidence for lowering it, because
what is missing from the measurement is exactly what would be missing from the
recommendation. `backend-requirements.md` states this as an obligation, because
a wrong right-sizing recommendation costs more than an unsaid one.

## Consequences

**Easier.** Memory right-sizing gains a bound on day one instead of after a week
of histograms, and ADR 0047's `GOMEMLIMIT` finding becomes actionable: "not set,
and the peak is at 92% of the limit" names a service, where "not set" named half
the fleet. The gap ADR 0006 recorded as accepted is partly closed, without
changing how usage is collected at all.

**Harder / given up.** The public sentence about what the scanner reads is
longer, and a list is something a reviewer must check rather than take on faith
— the same trade ADR 0047 made for `env`, and mitigated the same way: two named
fields, in one function, enumerated in the customer document.

The measurement covers Go processes in collected pods and nothing else, because
that is what the scanner keeps. A container whose memory is held by a sidecar,
or by a non-Go process, reports a peak that is a floor with more missing under
it than usual.

The right counterpart is cgroup v2's `memory.peak`, which is the number the
limit is actually enforced against. Reading it means reading cgroupfs, a
different access class with its own argument to make, and it is not made here.

**Not changed.** No capability is added and no RBAC is touched. The scan
interval, the scope query, the channel's authentication and the spool's bounds
are all as they were; this is one more file opened per already-kept process.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
