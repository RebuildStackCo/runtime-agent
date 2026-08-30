# 0053. The network counters are the pod's, and they say nothing about where the bytes went

Date: 2026-08-29

Status: Accepted

Amends: 0006 §3, 0014

Adds the payload kind `network_window` from `PodStats.Network`, a field already
in every kubelet summary the agent parses. No new RBAC, no new kubelet path, no
new privilege.

## Context

The agent reads `/stats/summary` on every poll and uses two thirds of it. The
network block has been in the response since the beginning and has never been
looked at.

What it enables is not a new finding on its own — it is a multiplier on one that
already exists. [ADR 0051](0051-topology-routing-is-asked-and-not-granted.md)
made the agent able to say that a Service asked for zone-local routing and the
cluster declined to arrange it. It cannot say what that costs. A workload moving
forty gigabytes a day across zones and one moving four megabytes produce the same
finding today, and they are not the same item on anyone's list.

It also answers a question customers reliably get wrong on their own: which
workload actually moves the most traffic. The usual answer is the observability
stack, and the usual reaction is surprise.

## Decision

**1. The counters are the pod's, so the record keys on the workload.** Every
container in a pod shares one network namespace, and the kubelet counts there.
There is no container to attribute the bytes to.

This is why they are not nested onto the usage records under
[ADR 0014](0014-scoped-facts-are-nested.md)'s rule. That rule works for facts
nobody adds up — `pod.replicas` repeated across three container records is
self-evidently not nine replicas. Bytes are different: `backend-requirements.md`
§4 tells the backend that summing container records gives the pod's total and
that this is the intended use. Pod-scoped bytes repeated on each container
record would make that documented operation return double for a two-container
pod. A separate kind, keyed `(namespace, workload kind, workload name)`, is the
only shape that does not break a promise already made.

The counter discipline is ADR 0006's, unchanged: deltas from the timestamps
inside the response rather than from scrape time, a re-served snapshot
discarded, a counter running backwards treated as a recreated sandbox and
rebaselined. One simplification falls out — a pod's network namespace lives as
long as the pod, so a container restart does not reset these, and the baseline
is keyed per pod rather than per container.

The first observation of a pod emits nothing. A container's stats carry a start
time, so its first counter value is a delta since it started; the pod's network
block carries no such thing, and attributing a pod's whole lifetime of traffic
to the window it was first seen in would be a fabricated spike.

**2. `hostNetwork` is reported, because for those pods the counters are the
node's.** A pod sharing the node's network namespace gets the node's interface
statistics — real numbers, describing the machine and everything else on it.
Summed across a DaemonSet's pods they attribute the entire cluster's traffic to
one workload.

The flag ships on the record and the contract forbids folding those records in
with the rest. The agent does not drop them: on a node whose traffic is
dominated by one host-network pod the number is close to the truth, and which
case this is belongs to whoever has the rest of the picture (ADR 0004).

**3. Interfaces are summed; their names are not read.** An interface name is a
description of the cluster's network topology. What ships is the count, which is
what says a second interface exists at all. The kubelet repeats the default
interface both inline and in the list when it reports the list, so the list is
preferred where present rather than added to the inline copy.

**4. Closed windows only, and no snapshot sibling.** Usage ships an open-window
snapshot because something consumes it — the profiling targets are ranked from
it — and because a partial CPU total is still a rate over a known covered time.
Neither holds here: nothing inside the agent reads network volume, and a partial
window of bytes read as a rate over the whole window is simply wrong. So this
kind has one shape and one file per window, and the newest hour is not visible
until it ends.

**5. Absence is not zero, and the distinction matters more here than
elsewhere.** Whether the network block is populated depends on the container
runtime and the CNI, and there are real clusters where it is empty. `samples`
counts the pod observations that carried counters at all: zero bytes with a
sample behind them is a quiet workload, zero bytes with no sample is a cluster
that does not report this (ADR 0013). The `network` signal joins the observation
block only when a real block was seen.

## Consequences

**Easier.** The topology-routing finding gets its multiplier, and the report can
name the three Services worth fixing rather than the twelve that match. "Your
log shipper is the largest network consumer in the cluster" becomes sayable, and
it is the finding customers least expect. The error counters come along in the
same struct at no cost and are the only network health signal the agent has.

**Harder / given up.** Three limits, and they bound what may be claimed:

*No destination.* The kubelet counts bytes at the pod's interface and never says
to whom. Cross-zone transfer cost cannot be computed from this — only ranked by
candidate. A backend rendering a cross-AZ figure from these numbers would be
inventing it, and `backend-requirements.md` says so.

*No per-container attribution.* The shared namespace means a sidecar's traffic
is the application's traffic. The "sidecar tax" this field was once wanted for
cannot be had from it, and that is a reduction against what was originally
proposed.

*The window is coarse.* Only closed windows ship, so the first record for a new
workload arrives when its first window ends.

**Not changed.** No RBAC, no new kubelet path, no new privilege — this is the
last unread field of a response the agent already pays for. The window
arithmetic is the one already tested for the merge property: the alignment and
pro-rata split moved into a type both accumulators use rather than being copied,
because a second copy would drift from the one the property tests cover.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
