# 0042. The spool is bounded, and its bound does not depend on the cluster being healthy

Date: 2026-08-26

Status: Accepted

Amends: 0007 §4

Gives the spool a size and file-count ceiling, moves the sweep onto the agent's
own cadence, and stops a caller-supplied string from reaching a filename. No
change to what is collected, to any payload's bytes, or to any RBAC rule.

## Context

ADR 0003 made the spool a directory of files whose names are natural keys, with
no index, and ADR 0007 gave it a maximum age so that "an extended outage
degrades to memory-only behavior instead of filling the volume". Three things
were wrong with that as a bound, and together they are the one remaining audit
finding whose victim is the customer's cluster rather than this product.

**The age cap was conditional on the cluster being healthy.** `sweep` was
private and called from exactly one place — the end of `WriteUsageSnapshot` —
with the comment that it rode the snapshot cadence "so no extra timer exists".
But `WriteUsageSnapshot` runs only when there are usage records, and there are
none when the kubelet cannot be polled: the `nodes/proxy` grant withheld, every
kubelet unreachable, or the poll loop stuck. On exactly those clusters nothing
was ever swept, the 24h cap was never applied, and the spool grew without limit
while the agent looked healthy. A bound that stops working on an unhealthy
cluster is not a bound; it is a bound-shaped comment.

**Age bounds nothing an adversary controls.** The spool's file count follows
cluster events, and `WriteOOMKill` writes one file per
`(finishedAt, namespace, pod, container, restartCount)` — the restart count is
in the name, so every restart is a new file rather than an overwrite. A pod
OOM-killing in a tight loop produces thousands per minute. Nothing acknowledges
payloads yet, so nothing removes them before the 24h mark. The controller's
`emptyDir` had no `sizeLimit`, so this consumes the node's ephemeral storage,
and a node that runs out of ephemeral storage makes the kubelet start evicting
pods — other workloads' pods, on a node this agent is a guest of.

The irony is local: the restart *journal* was deliberately windowed with the
reasoning that "a crash loop must not put the spool's file count under its own
control". That reasoning was never applied to the OOM path beside it.

**A caller-supplied string reached a filename.** `WriteProfile` built its name
from `key.Workload`, which is `ownerReferences[].name`. The API server validates
that field for non-emptiness and nothing else — no DNS-1123, no character set —
and `filepath.Join` resolves `..`. So a pod created with a crafted owner
reference, allowed to become a profiling target, directed a payload write out of
the spool directory.

How far it got is worth stating precisely rather than dramatically: the write
creates no intermediate directories, so it lands only where the whole parent
path already exists and is writable. In the shipped chart
`readOnlyRootFilesystem: true` makes most targets fail with EROFS. That is a
real mitigation and it is an accident of an unrelated setting, not something
this package did. The namespace, pod and container names beside it are DNS-1123
and were safe for the same kind of reason — luck about which Kubernetes field is
validated.

## Decision

**1. `Sweep` is exported and called on the agent's own cadence.** It runs from
the periodic loop that already flushes metadata and journals, unconditionally,
whether or not a usage record was written. The second timer that the old comment
was avoiding does not exist: the timer was already there.

**2. The spool has a size ceiling and a file-count ceiling, enforced
oldest-first after the age cutoff.** 512 MiB and 20000 files, as constants
rather than values — the spool is always an `emptyDir` and no setting changes
that (ADR 0026), so a knob here would be a promise about a volume the operator
does not choose. Both are needed: the smallest payload is a few hundred bytes,
so a byte budget alone permits millions of files, which exhausts inodes and
makes every sweep's listing expensive.

Oldest-first is the right loss, and it follows from ADR 0007 rather than being a
new claim: every payload here is loss-harmless, and the oldest is the one most
likely to have been superseded already.

**3. The `emptyDir` gets a `sizeLimit`, deliberately larger than the agent's own
budget.** 1 GiB against the agent's 512 MiB. The ordering is the point: the
agent's bound is the one meant to act, and the kubelet's is what holds if the
agent's ever fails. Asserted in the chart test as a relation to the constant,
not as a number, so the two cannot drift into the wrong order.

**4. Every caller-supplied component of a filename goes through `fileToken`.**
Not just the workload name. Which Kubernetes fields are validated is not this
package's business to track, and "safe because that field happens to be
DNS-1123" is the reasoning that made this a finding rather than a bug.
`fileToken` keeps `[A-Za-z0-9._-]`, maps everything else to `-`, bounds each
component, and refuses a component of only dots — `.` and `..` are directories,
not names.

**5. `write` refuses a name that is not a plain filename.** It catches nothing
today, since the callers already sanitize. It is there because every payload
this package writes goes through that one function, which makes it the only
place worth checking, and because the next caller will not read this ADR.

## Consequences

**Easier.** The spool cannot fill a node, on any cluster, healthy or not. The
agent stops being able to cause an eviction of somebody else's workload, which
is the only way this product could have harmed the cluster it is measuring.

**Harder / given up.** A crash-looping pod can still push useful payloads out of
the spool: it produces OOM payloads faster than anything else is written, and
oldest-first eviction drops the older usage windows first. The data is
loss-harmless and the alternative — ranking payload kinds against each other
during eviction — is a policy with no obvious right answer and no way to test it
honestly. Named here rather than solved.

Windowing OOM events the way restarts are windowed would remove the pressure at
its source, and is not taken here: it changes a payload's shape, which is a
protocol decision and belongs in its own record.

The sweep lists the directory on every tick. With the count now bounded at 20000
that is a bounded cost, where before it was proportional to whatever had
accumulated — so this decision also makes the sweep's own cost bounded, which it
was not.

**Not changed.** No payload bytes, no golden file, no RBAC rule, no collection.
`fileToken` shapes filenames only: the payload still carries the workload's real
name, unmodified, because what a backend receives is not a filesystem question.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
