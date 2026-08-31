# 0061. What the process holds, beside the peak it once reached

Date: 2026-08-31

Status: Accepted

Amended by: 0062

Amends: 0052

Adds seven fields to `process_peaks`, read from three files under
`/proc/<pid>/`. No new privilege, no new payload kind, no new RBAC, and no
identity of anything.

## Context

ADR 0052 established the node's second read: `/proc/<pid>/status`, for `VmHWM`
and `Cpus_allowed_list`. That file has about forty rows and the scanner takes
two of them, because the allow-list was drawn for one finding.

Three questions the agent cannot answer today are answered by rows in that same
already-open file, or in two small neighbours:

- **How much of a container's memory is the program's own.** A memory limit
  counts anonymous pages and file-backed pages alike, so a service whose 800 MB
  is 300 MB of heap and 500 MB of page cache looks identical to one that
  allocated all of it. Advice about limits rests on the difference, and the
  agent has been giving the total.
- **What one more replica actually costs.** Resident memory counts a shared page
  once per process mapping it. Replicas of one image share their text, so
  summing RSS over replicas overstates what they occupy, and the figure that
  does not is `Pss`.
- **How close a process is to a ceiling it will hit without warning.** Descriptor
  exhaustion is not visible in any metric until connections start failing.

None of these needs a mechanism the agent does not have. The scanner already
walks the process, already opens `status`, already passes the kernel's ptrace
check to do it.

## Decision

**1. The status allow-list widens by three rows.** `RssAnon` and `RssFile` split
resident memory into the program's own pages and the file-backed ones; `Threads`
is the process's OS threads, which for a Go program is a consequence rather than
a setting — the runtime creates one per blocking syscall it cannot park, so a
count far above `GOMAXPROCS` is the signature of blocking or cgo work.

The list stays an allow-list. `Name` is still line one of every status file and
still has nowhere in the struct to go, which is the property the parser's test
exists to keep true as the struct grows.

**2. `smaps_rollup` is read for `Pss` and `Private_Dirty`.** The rollup is the
kernel's own aggregate over every mapping, so this is one small file rather than
a walk of `smaps`, which on a large address space runs to thousands of lines.
Keys are matched exactly: `Pss_Dirty` sits directly under `Pss` and
`Private_Clean` directly above `Private_Dirty`, so a prefix match would be wrong
by a plausible amount, which is the worst kind of wrong.

This is the one read in the pass with a cost the others do not have: the kernel
walks the page tables to compute `Pss`. That cost is not measured, and by
ADR 0039 it is therefore marked rather than claimed — see Consequences.

**3. Descriptors are counted against the ceiling in force.** The entries of
`/proc/<pid>/fd` are counted without being followed: what each points at is a
path, a socket or a pipe, which is an identity. The soft `RLIMIT_NOFILE` comes
from `/proc/<pid>/limits` in the same pass, so the headroom between the two is a
fact about one process at one moment rather than a subtraction across two
readings (ADR 0043's discipline). An unlimited ceiling reports as absent, never
as a very large number that would read as a real one.

**4. The new fields merge as maxima, except the ceiling, which merges as a
minimum.** Everything else in this record is the largest value seen across the
processes of one build on every node still running it. The descriptor limit is
the exception because it is not a measurement but a bound: the honest pairing is
the most descriptors any replica held against the lowest ceiling any replica was
given.

The field names say which kind of claim each is. `peak_rss_bytes` is a mark the
kernel maintains between readings; every `*_max` beside it is the highest value
the agent happened to sample. Naming them alike would have made the payload
claim a continuity it does not have.

**5. A process that measured nothing contributes nothing.** Unchanged from
ADR 0052 and worth restating because it now covers more fields: a zero is not
folded in, and such a process is not counted in `processes`, which says how many
processes stood behind the numbers rather than how many ran.

## Consequences

**Easier.** Memory advice can distinguish the heap from the page cache, which is
the distinction that makes a limit safe to cut. The marginal cost of a replica
becomes a number rather than an assumption. Descriptor exhaustion becomes a
prediction instead of a post-mortem.

Three of the seven fields cost no new file: they are rows of a file the scanner
already opens and parses.

**Harder / given up.** `smaps_rollup` is read per kept process per pass and its
cost scales with the process's address space. It is bounded by the same loop
that bounds everything else in the pass, and a missing file — a kernel before
4.14 — is the zero value rather than an error. But the cost itself is unmeasured
on a real node, and this ADR states that rather than implying a measurement that
was not taken. The way to close it is the inventory end-to-end run on a cluster
with large processes, timing the pass with the read and without.

The record now mixes two kinds of memory claim under one key. The names carry
the distinction; a reader who ignores them will average a kernel-maintained mark
with a sample.

**Not changed.** No new capability: the node already runs with `hostPID` and
`CAP_SYS_PTRACE` to open `/proc/<pid>/exe` (ADR 0037), and every file here sits
behind the same check. No identity leaves — the descriptor entries are counted
and not followed, the rollup's mapping paths are not read, and the process name
still has nowhere to go.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
