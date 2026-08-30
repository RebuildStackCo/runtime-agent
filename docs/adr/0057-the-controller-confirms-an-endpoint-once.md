# 0057. The controller confirms an endpoint once per build, and that is its first connection to a workload

Date: 2026-08-30

Status: Accepted

Amends: 0054

Adds `/debug/pprof` endpoint discovery to the controller: the first outbound
connection the agent makes to anything but the API server. Removes the `pprof`
installation profile, which never existed. No new RBAC, no new read of the API.

## Context

[ADR 0056](0056-a-pprof-endpoint-is-proved-not-probed.md) put two facts in the
controller's hands: which builds link `net/http/pprof`, and which ports their
processes bind. Neither says whether anything actually serves the handler. A
service may import the package for its side effect and never run a mux on
`DefaultServeMux`, and a binary that does serve it does so on one of its ports,
not all of them.

That last question is not answerable from the node. It is answerable by one
request, and the whole design is about making that request rare, bounded, and
about the right subject.

The other thing to settle is what `pprof` is. `security.md` §3 has listed it as
an installation profile since the document was written, and the chart has
refused to install it. Both were wrong about the shape of the thing:
`/debug/pprof` discovery needs the node's binary read and the node's socket
read, so it is part of `inventory`, not an alternative to it. It is a switch,
not a profile, and the profile is deleted the way ADR 0055 deleted
namespace-scoped installation.

## Decision

**1. The subject of a probe is a build's port, not a pod.** Every replica of an
image serves the same page, so asking a second one learns nothing. A
fifty-replica Deployment costs one request, and a cluster costs one request per
distinct (image digest, port) that survived the funnel.

Three stages run before any byte of network, and each can end the question for
free: the workload passed the collection filters, its build carries the marker,
and the port came from the process's own socket. A loopback port is dropped
rather than asked about — nothing outside the pod can open it, so the answer
would describe the agent's location rather than the workload.

**2. Two of the three answers are terminal, and the third is not.** A port that
served the index is confirmed; a port that answered with something else is
absent. Neither can change under a fixed digest, so neither is asked again — and
absence is the common case, since a workload that links the package serves it on
one port out of several.

A connection that failed is a fact about the network, not the endpoint: a
NetworkPolicy, a mesh, a pod mid-restart. It expires and is retried.

Confirmation matches the title Go's own index page carries, not the status code.
A 200 alone proves that something answered on the port, which is what a router
with a permissive fallback does.

**3. The pod IP is a connection parameter, not a fact.** The agent has held pod
addresses in its informer cache all along and has never read one; the cache
transform strips them from EndpointSlices precisely so they cannot be counted
(ADR 0046). This decision reads one, at the moment of connecting, from the
lister — and it enters no index, no record and no payload. `PodWatcher.PodAddress`
is the only place it is read, which is what makes that statement checkable.

**4. It is on by default, and that is the exception it looks like.** Every other
collection control in this agent defaults to the narrower setting. This one does
not, for a reason specific to what it costs: the request fetches a page rendered
from memory, starts no profiler, holds no connection, and asks once per image.
There is no meaningful sense in which the workload pays for it.

The argument that would keep it off is that an upgrade must not silently change
behaviour. That argument is sound in general and empty here — the transport and
identity of ADR 0008 are unbuilt, so there is no installation shipping anything
to a backend to surprise. By the time there is, this will have been the default
from the beginning.

What the default costs instead is a documentation obligation, and it is
discharged in the same pull request: `security.md` §10.3 stops describing a
plan and describes what the agent does, before anyone installs it. A customer
who learns this from the document has agreed to it; one who learns it from an
unexpected connection has not.

**5. There is no egress NetworkPolicy, and that is a decision rather than an
omission.** The existing policy's comment deferred egress restriction to "the
change that gives it something to egress to". This is that change, and the
answer is no.

An egress rule would have to permit the API server — a ClusterIP that is not
knowable when the chart renders — plus DNS, plus arbitrary pod addresses on
arbitrary ports, plus a backend later. That set is everything, and a rule
permitting everything is a boundary in name only. Writing it would repeat the
mistake ADR 0039 was written about: a control that reads as a limit and is not
one.

What bounds the egress is the code, and the bound is the kind that can be
checked: one package opens outbound connections, to one path, at addresses of
pods the filters admitted, on ports the processes were observed to bind. This
is the same form the `nodes/proxy` disclosure takes in `security.md` §5 — the
grant is wide, the call sites are counted.

**6. The path is an allow-list of one, and the reason is not caution.** The
pprof mux serves `/debug/pprof/cmdline`, which returns the process's own argv —
the field CLAUDE.md invariant 4 drops at the source, in the informer cache
transform, on the node, and everywhere else. An endpoint that hands it over on
request is not a reason to collect it. `TestOnlyTheIndexPathIsRequested` asserts
that the only path requested is the index.

## Consequences

**Easier.** "Nine of your services can be profiled right now, and three of them
expose a debug handler on a routable address" becomes sayable before anything
is pulled. The first of those is the input to a puller; the second is a security
finding that needs no puller at all.

The counters land in `collection_coverage` (ADR 0054), so "no profiles for this
workload" separates into three causes: never confirmed, confirmed-absent, or
unreachable. Without them the absence would be the indistinguishable silence
that payload exists to end.

**Harder / given up.** The agent now connects to customer workloads. That is a
real change in what it is, however small each connection is, and it is the
reason §10.3 and the values file both had to be rewritten rather than amended.

A confirmed endpoint is not a profile. The request that starts a profiler in the
customer's process is a separate decision with its own ADR, its own default, and
a different risk — this one holds nothing open and changes nothing.

Discovery is inert without a node DaemonSet. Both facts it funnels on are read
under `/proc`, so a `metrics-only` installation finds nothing, and the chart
renders the switch off there rather than leaving it to look enabled.

**Not changed.** No RBAC, no new API read, no capability. The node role still
holds no Kubernetes identity, and the `create` verb still appears nowhere.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
