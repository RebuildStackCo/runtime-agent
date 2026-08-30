# 0056. Whether a pprof endpoint exists is proved on the node, not probed over the network

Date: 2026-08-30

Status: Accepted

Amends: 0009

Adds the payload kind `listening_ports` and one field to `go_build`. Widens the
node scanner's reads to the executable's function-name table and to the
process's own socket descriptors. No new capability, no new RBAC, no network
request to any workload.

## Context

Pulling CPU and heap profiles from services that already import
`net/http/pprof` needs no eBPF, no `CAP_BPF` and no DaemonSet of its own. What
it needs first is an answer to "which workloads have such an endpoint, and
where" — and the obvious way to get that answer is to ask the workloads, one
HTTP request per pod per candidate port.

That way is bad in three separate directions. It is a port sweep from inside the
cluster, which `security.md` §10.3 has warned about since it was written. It
scales with pods rather than with images, so a fifty-replica Deployment costs
fifty requests. And it cannot prove absence: a pod that does not answer may have
no endpoint, or a different port, or a NetworkPolicy in front of it, or it may
have been restarting. Every negative has to be retried forever.

The node already opens the executable of every process it keeps (ADR 0009) and
already reads two files under `/proc/<pid>` (ADR 0052). Both halves of the
question are answerable there, offline, and one of them is answerable as a
proof rather than an observation.

## Decision

**1. The linked package is the evidence, and it is read from the binary.** A Go
binary either has `net/http/pprof` compiled in or it does not. The scanner
searches the executable for the function name `net/http/pprof.Index` and reports
the result as a build fact on `go_build`.

Measured 2026-08-30 on four binaries — importing and not, stripped and not: the
name is present exactly in the two that import the package, and it survives
`-ldflags="-s -w"`, the ordinary production build. It survives because it lives
in the table the runtime needs for tracebacks, not in the symbol table a strip
removes. `TestTheMarkerSurvivesTheLinker` builds all four and asserts it, so the
claim is checked by CI rather than by this paragraph.

The whole name is matched, never the substring "pprof": the negative binary in
that measurement contained "pprof" anyway, because the recorded path of its own
source file passed through a directory named after what was being tested.

False is therefore a **proof of absence**, and that is the property the design is
for. It ends the question for that image rather than scheduling a retry.

**2. Ports come from the process's own descriptors, and only listening ones are
parsed.** `/proc/<pid>/net/tcp` and its IPv6 sibling are the network namespace's
socket table, which a pod's containers share. A row is kept only when the
process holds the descriptor for it, resolved through `/proc/<pid>/fd`.

This is what makes the answer the workload's rather than its neighbours'. It is
also what makes a host-network pod safe to read without a special case: in the
node's namespace the other listeners belong to other processes, and they are
never attributed to the workload.

Rows are filtered on the state field, before an address is parsed. Every
accepted or outgoing connection — which is where peer addresses are, and with
them the cluster's connection graph — is discarded on that field. The agent
therefore never holds a remote address at any instant, rather than holding one
and dropping it (CLAUDE.md invariant 4). A listening socket's local address is
reduced to one bit, loopback or not: an endpoint bound to `127.0.0.1` is
unreachable from outside the pod, which is worth knowing, while a socket bound to
the pod's own IP would carry that IP, which is not collected.

**3. `listening_ports` is its own kind, keyed by build, rebuilt from the latest
report of each node.** Not a field on `go_build`, because a port is not a
property of an image: the same build serves different ports under different
configuration. Not a field on `go_inventory`, because it changes on its own
schedule. And not nested onto anything, for ADR 0014's reason — it has its own
natural key.

The staleness discipline is ADR 0052's, for the same failure: a node's
contribution is replaced wholesale by its next report, and the record dies when
no node still stands behind it. A port a process stopped binding is not a port
the workload serves, and a stale one would send a future reader after an
endpoint that is gone.

**4. The cost is paid per binary, not per process per pass.** The marker search
reads the whole executable. Measured 2026-08-30 with a warm page cache: 1.3 ms
for 5 MB, 18 ms for 61 MB. At the default 60-second scan interval, repeating
that for every process of every pass is a real cost on a busy node, so the answer
is cached against the executable's identity and dropped when no live process runs
that file. The cache is bounded by what runs on the node, never by uptime.

This is the same shape as the rest of the design: the unit of work is the image,
because the question is about the image.

## Consequences

**Easier.** The candidate set for a future puller is computed offline: passed the
filters, is Go, links the package, binds a reachable port. Three of those four
come from data the node already had. Whatever probing remains is over distinct
images rather than pods, and it never has to re-ask about a negative.

Two findings arrive without a puller existing at all, which is why this ships on
its own. A debug endpoint compiled into a service and bound to a non-loopback
address is a security finding whether or not anyone profiles it. And the ports a
workload actually binds, set beside the `containerPorts` it declares, disagree
often — a declared port nothing serves, or a served port nobody declared, and the
second is the one no reader of the spec can see.

**Harder / given up.** The marker proves the package is linked, not that anything
serves it: a service that imports it for its side effect but never runs a mux on
`DefaultServeMux` reports true. The remaining uncertainty is exactly the part a
single request settles, and settling it is a later decision with its own ADR.

Reading `/proc/<pid>/fd` is a new directory the node walks. It sees the targets
of a process's descriptors, which include the paths of files it has open;
nothing but `socket:[…]` targets is examined, and no path leaves the function
that reads them. It is bounded, and a process holding more descriptors than the
bound reports no ports rather than being walked.

An unreadable executable and an executable without the package are the same
answer, false. The distinction would matter if false triggered work; it ends
work instead, so the cost of the conflation is a missed capability, never a
wrong claim.

**Not changed.** No capability, no RBAC, no request to a workload. The node role
still holds no Kubernetes identity, and the controller still opens no connection
to a pod.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
