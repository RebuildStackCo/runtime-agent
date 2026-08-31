# 0060. The node says what its profiler did, on the channel that fires when it did nothing

Date: 2026-08-31

Status: Accepted

Amends: 0011 §2, 0054

Adds an `ebpf` block to `collection_coverage` and a `profiling` block to the
node→controller scan report. No new read, no new privilege, no new route, and
nothing that names an object.

## Context

ADR 0054 exists so that an empty report and a broken agent are not the same
bytes. Every collector answers for itself: the filters count what they excluded,
the node scanners count what they walked and skipped, the endpoint prober counts
targets by their latest answer, the puller counts refusals apart from failures.

The eBPF capture path answered for nothing. Its whole vocabulary of failure
lived in the node's log, or only in memory:

- the readiness gate's refusal — `kernel_too_old`, `btf_absent`,
  `kernel_unknown` — counted in a map ADR 0011 left with a note saying a
  consumer would surface it, which was never written;
- the eBPF program failing to load;
- a window dropped because the controller supplied no scope, which is the
  fail-closed path of ADR 0015 and the one most likely to be a misconfiguration;
- a window with no targets, and a loaded profiler that captured no sample at
  all — a broken profiler and an idle node, indistinguishable;
- a profile `nodeprofile.Validate` refused;
- what the symbol filter redacted, discarded at the call site into `_`.

That last one is not a small omission. A profile of nothing but the runtime and
placeholders is exactly what the defect ADR 0058 and ADR 0059 fixed looked like
from outside the node: the customer's own service, classified as third-party and
redacted, arriving as silence. It was found by reading the filter, not by
noticing anything, because nothing counted it.

On the controller, two atomics counted profiles received and profiles that could
not be joined to a pod, and both only reached a log line.

The asymmetry is the argument. The node already ships counters — the scan block
of `collection_coverage` is built from them. Same DaemonSet, same process, same
channel, every pass. The scanner half of the node explained its blindness; the
profiler half beside it did not.

## Decision

**1. The counters ride the scanner's report.** `nodescan.Report` gains a
`profiling` block, and the controller's store keeps it per node beside the scan
counters it already keeps.

Attaching them to the profile report instead is the obvious move and it is wrong
in exactly the interesting case: a node whose gate refused ships no profile, so
the report carrying the reason would never be sent. A third route was rejected
for a struct that fits in the one already sent every pass.

**2. What is counted, and what is a state rather than a count.** A node has one
profiling state — `supported`, `disabled`, `program_load_failed`, or a gate
refusal — evaluated at startup, so it is a field and not a tally of events. The
fleet view of it is a map from state to how many nodes are in it, which is what
answers "why is this cluster not profiling" in one field.

The rest are counts: windows cut, and the three ways a window produced nothing
(no scope, no targets, no samples), in that order of precedence because it is the
order that separates the agent from the cluster. Then what became of each profile
built — shipped, invalid, undelivered — the samples dropped for being outside the
controller's own scan scope, and the filter's three drop counters folded in.

The two the controller counts itself, profiles received and profiles unjoined,
travel in the same block: a profile a node captured and the controller could not
attribute is a loss the node cannot see.

**3. Cumulative on the node, latest-wins on the controller.** Unlike the scan
counters beside them, which describe one pass, these describe everything the node
has done since it started. A report is then a statement, not an increment: a lost
one costs staleness and never a count, and the fleet answer is the sum of the
latest statements. The node keeps no state across restarts (ADR 0003), so a
restart re-bases its counters, and the fleet sum can fall without any work having
been lost.

This does mean two blocks in one payload with different time bases — `scan` is
the last pass, `ebpf` is since each node started, neither of them the payload's
own `since`. That is stated in `backend-requirements.md` rather than smoothed
over by making one of them lie.

**4. Aggregate, never per node.** For the reason ADR 0054 §4 gives: the question
is asked of the fleet, and a per-node breakdown grows the payload with the
cluster. So the payload says four nodes refused for want of BTF; it does not say
which four. Node names are already in `node_metadata`, and joining a refusal to
one is not a question anybody asks — but it is also not one this payload can be
made to answer.

## Consequences

**Easier.** "This workload has no eBPF profile" is now legible as one of several
different things, from the payload alone, without asking the customer for the
logs of a DaemonSet.

The kernel distribution of the installed base becomes visible — `kernel_too_old`
against `btf_absent` is the difference between waiting and doing something —
which is what ADR 0011 said the split of reasons was for, and it never left the
node until now.

The class of defect ADR 0058 and ADR 0059 closed acquires a symptom. An
allow-list that stops matching shows up as third-party drops rising and invalid
profiles rising, on a payload someone reads, rather than as a quiet absence.

**Harder / given up.** The payload grows by about fifteen numbers, and a reader
of two adjacent blocks has to know their bases differ. A node restart makes the
fleet sum fall, which is not a decline in work and must not be read as one.

A profiler that dies after a successful load has no state to change: it shows up
as windows accumulating with no samples, which is the same shape as an idle node
and is distinguished only by the cluster's usage records saying otherwise.

**Not changed.** Nothing new is read on the node, no privilege is added, and
every field here is a count or a low-cardinality reason. No identity of a
redacted frame, an out-of-scope pod or a refusing node appears (CLAUDE.md
invariant 6).

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
