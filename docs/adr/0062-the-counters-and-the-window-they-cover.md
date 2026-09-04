# 0062. The counters are subtracted on the node, and never leave without their interval

Date: 2026-08-31

Status: Accepted

Amends: 0052, 0061
Amended by: 0067

Adds the payload kind `process_counters`, and one piece of node-held state: the
previous pass's counter reading per process. Reads three more files under
`/proc/<pid>/` and two more rows of one already open. No new privilege.

## Context

Everything ADR 0052 and ADR 0061 collect is a value: a peak, a footprint, a
count of threads. The rows beside them are a different kind of thing — counters
that only rise, since the process started. `run_delay` says how long a process
has waited for a CPU in its whole life, and a life is not a question anybody
asks. What a finding needs is the change over an interval.

That subtraction has to happen somewhere, and where it happens is the decision.
Shipping raw counters and subtracting on the controller was the first idea and it
fails on two counts. A PID is reused, so two readings under one number can belong
to two processes, and the controller has no way to tell — it never sees a start
time, and holding per-PID state for a fleet's processes would grow the store with
the cluster rather than with the workloads. And a process that exits between two
flushes has its last interval lost, because the controller's previous reading is
the only thing that would have closed it.

The node has both halves: it reads the counters and the start time from the same
file at the same moment, and it already keeps a cache across passes for the
binary marker (ADR 0056).

## Decision

**1. The node subtracts, and the start time keys the subtraction.** A cache holds
one reading per PID between passes — the counters, the instant they were read,
and the process's start time. A pass compares against the previous reading only
when the start time matches; otherwise the PID belongs to a different process and
there is nothing to subtract from.

Three cases produce no delta rather than a wrong one: the first pass that sees a
process, a PID that has been reused, and a counter that fell. The last should be
impossible when the start time matches, and it drops the whole delta rather than
clamping one field, because a counter that fell means the reading is not of the
process the last one described, whatever the start time says.

Field two of `/proc/<pid>/stat` is the executable's own name, and it may contain
spaces and parentheses. Fields are counted from the *last* `)` rather than by
splitting the line — which is also why the name never reaches a variable here:
it is behind the cut, not filtered after it (the discipline ADR 0052 set for
`status`).

**2. A delta never travels without the interval it covers.** Each carries
`observed_nanos`, the wall-clock time between the two readings. A delta alone is
a number no consumer can turn into a rate, and the interval is not a constant:
a pass can be late, a report can be lost, and a node restarts.

**3. The payload carries sums, never ratios.** `on_cpu_nanos` beside
`run_delay_nanos` is CPU starvation stated as the two numbers it is made of. The
agent could divide them, and does not: a payload that ships the division cannot
be re-divided by a consumer asking a different question, which is the shape
ADR 0004 settled for money and holds for every derived figure.

`observed_nanos` sums across processes, so it is process-time and not a duration.
Two processes observed for a minute each contribute two minutes. That makes it
the denominator of a rate and not a window, and it is named for what it is.

**4. A node's next report replaces its interval, it does not extend it.** The
same wholesale replacement ADR 0052 §3 uses for peaks, for a different reason:
adding successive intervals would turn a rate into a total over a window nobody
chose, drifting further from any wall-clock boundary with every pass. The payload
is therefore what the fleet's processes did over the most recent interval each
node measured — ragged across nodes by construction, and honest about it, because
each record carries the process-time it rests on.

**5. Two rows come from a file already open.** The context-switch counters are in
`/proc/<pid>/status`, which the scanner reads for the footprint. They are
monotonic like the rest, so they join the reading rather than the footprint.

## Consequences

**Easier.** CPU starvation becomes measurable without depending on CFS
throttling being visible: `run_delay` counts the wait even when the quota was
never hit, and the non-voluntary switch count is the same fact from the
scheduler's side. The user/system split says whether a service burns its own
code or the kernel acting for it. Major faults separate a working set that does
not fit from one that is merely large. Disk bytes per workload exist at all,
which they do not in any kubelet reading.

**Harder / given up.** The node now holds state between passes that it did not
before: one reading per live process. It is bounded by the process table and
dropped for every PID a pass does not see, and losing it costs one interval and
never a total (ADR 0003). But it is state, and a node that restarts every minute
would produce no counters at all — which is visible, because the record's
`processes` count would stay at zero.

The interval between a process's last observed pass and its exit is lost. There
is no way to close it without the node observing the exit, which would mean
watching every process rather than sampling the table.

`processes` in this record is lower than in `process_peaks` for one interval
after a pod starts, because a process seen once has a footprint and no delta.
The two counts answer different questions and are not the same number.

**Not changed.** No new capability — `/proc/<pid>/io` sits behind the same ptrace
check the node already passes for `/proc/<pid>/exe` (ADR 0037). No identity: the
executable name in `stat` is behind the field cut, and what a byte was read from
is not in any of these files.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
