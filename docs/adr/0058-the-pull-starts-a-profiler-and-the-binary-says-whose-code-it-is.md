# 0058. The pull starts a profiler, and the binary says whose code it is

Date: 2026-08-30

Status: Accepted

Amends: 0011 §4, 0025, 0054

Adds the payload kind `pprof_profile`: a CPU profile fetched from an endpoint
[ADR 0057](0057-the-controller-confirms-an-endpoint-once.md) confirmed, reduced
to allowed frames in the controller. Moves the symbol allow-list into the
controller's configuration beside the node's. No new RBAC, no new capability.

## Context

Everything the agent has ever done is a read. A workload cannot tell it is being
collected: the kubelet counters, the object metadata, the binary on the node —
none of it touches the process. Pulling a CPU profile does. `GET
/debug/pprof/profile?seconds=N` runs the profiled process's own profiler for N
seconds, and Go allows exactly one CPU profile per process at a time.

That single slot is the whole of the risk, and the direction that matters is the
one people miss. If the customer's continuous profiler holds it, our request
fails and we lose nothing. If *we* hold it when their cycle starts, *their*
collection fails — and the gap appears in their system, attributed to nothing.

The second thing this decision had to settle was found while building it. The
symbol filter classifies a frame by module path: no domain means standard
library or unpublished code and is kept, an allow-listed prefix is kept, and
anything else is third-party and redacted. A customer's own service lives under
`github.com/acme/web`, which has a domain and is not on an empty allow-list. So
the shipped default would have pulled a profile, redacted the entire service
layer, kept `main` and the standard library, and thrown the result away as
unshippable. Cost to the customer, nothing in return — the exact trade
CLAUDE.md's fourth invariant exists to forbid.

## Decision

**1. Pulling is a separate switch from discovery, and both ship on.** They are
different events. Confirming an endpoint fetches a page from memory; pulling
runs a profiler. A customer who accepts the first and not the second says so
without losing the findings that rest on discovery alone.

**2. What the cluster pays is bounded by construction, not by a knob.** Ten
seconds per capture, one capture at a time, at most ten per five-minute round —
so the ceiling is a hundred seconds of profiling per five minutes across the
whole cluster, consecutively, however many endpoints exist.

Round-robin over the least recently profiled, not ranked by consumption. A
ranking profiles the same few workloads forever, and the finding a report most
often needs is about a workload nobody suspected. Coverage is the point;
ordering by suspicion defeats it.

**3. A refusal is respected for six hours and never fought.** A non-200 is
nearly always the process declining to start a second profiler, which means the
customer runs their own. The target is left alone for six hours: long enough not
to compete for the slot, short enough that switching that profiler off is
noticed within a shift.

There is no list of known profiler modules checked in advance. It was considered
and dropped: it guesses at what one attempt establishes, and the six-hour hold
already converges after a single harmless failure. What remains uncovered is a
narrow window — their profiler idle exactly when we asked — and on a ten-second
capture that is accepted rather than papered over.

**4. Filtering happens in the controller, and that is a real weakening, stated
rather than buried.** For a captured profile, ADR 0011 §4's guarantee is
physical: the raw stacks exist only in the node's memory and the filter runs
before anything crosses to the controller. A pulled profile arrives already
assembled, so the controller holds the unreduced stacks for the milliseconds
between the read and the filter. Nothing unreduced leaves the cluster, but "not
collected" has become "collected and dropped" for this one path, and the
document says so.

The allow-list therefore lives in the controller's ConfigMap as well as the
node's, from the same Helm value. It is still a file Helm owns and still not
something the wire can widen, which is what ADR 0011 §4's reasoning actually
rests on. That it sat only in the node's schema was a consequence of only the
node filtering, and ADR 0025's rule — a role's configuration holds what that
role enforces — is why it moves rather than being read from somewhere else.

A pulled profile also carries fields a captured one never had, and they are
removed with the frames: the mappings, which name the executable's path on disk
and its build ID; the sample labels, which are arbitrary strings the profiled
service attached and may hold anything; the comments; and the full source paths,
cut to base names as ADR 0041 requires. What is written out is rebuilt from what
survived, so a name that was redacted is not left behind in a table nothing
references.

**5. The build states which code is the customer's, and the filter believes it.**
`go_build` already carries each build's main module and marks every dependency a
`replace` directive redirected. The first is the service itself; the second is
what a monorepo builds from source rather than consumes. Both are read from the
binary, where no configured list can be wrong about them, and both are admitted
without configuration.

This is not a widening of what may leave. Those frames are the customer's own
code, which the allow-list exists to keep — the configured list was always meant
to name them and merely had no way to know them. It now covers the case it is
actually needed for: a shared internal module a build requires rather than
replaces.

**6. Every outcome is counted, and `refused` is the one to read first.** A
workload with no profile is otherwise indistinguishable from a broken agent. The
counters separate shipped, refused, unreachable and invalid, and a refusal
specifically means the customer's own profiler holds the slot — a fact about
their setup, not a failure of ours (ADR 0054).

## Consequences

**Easier.** "Thirty-one percent of this service's CPU is in JSON
serialization" becomes sayable, from a profile the workload's own runtime
produced, with no eBPF, no `CAP_BPF` and no privilege beyond what `inventory`
already has. And it works on a default install, because the binary says whose
code it is.

**Harder / given up.** The agent can now be noticed by a workload. That is a
change in kind, not degree, and it is why `security.md` §10.3 states it in the
document rather than in a values file.

The same allow-list defect this decision fixes for pulled profiles is still open
for captured ones. The eBPF path filters on the node against the configured list
alone, so a cluster running five services with one prefix configured redacts
four of them. The chart's refusal to install `ebpf` with an empty list bounds
the worst case and does not close this; the node knows each binary's modules
from its own scan, and wiring that into the node filter is separate work in a
separate ADR.

Heap profiles are not pulled. `/debug/pprof/heap` is a different kind, a
different claim, and a different promise, and this decision does not smuggle it
in.

**Not changed.** No RBAC, no new API read, no capability. The path allow-list
still holds one entry per package and `/debug/pprof/cmdline` is still named
nowhere. The eBPF capture path is untouched.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
