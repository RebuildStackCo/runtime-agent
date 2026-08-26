# 0015. The node asks the controller which pods it may scan, and fails closed

Date: 2026-08-20
Status: Accepted
Amends: 0009 §5
Amended by: 0039

## Context

`docs/security.md` §10.2 promises customers, in public, that node-level
visibility is mitigated by a collection-stage filter: *samples from processes
whose module path **or namespace** is not allow-listed are discarded on the
node, before transport to the controller.*

For the eBPF profiler that was true — targeting is deny-by-default and
controller-scoped (ADR 0011 §3). For the Go-binary scanner (ADR 0009) it was
not. The scanner walked every PID on the node, dropped only main modules on the
built-in infrastructure deny-list, and shipped every remaining binary's module
path, Go version, dependency list, pod UID and container ID to the controller,
where the namespace filters were applied for the first time — at the join.

So a customer who filtered the agent down to two namespaces still had the module
paths of every Go process on every node leave the node. The controller discarded
them at the join, and no payload was wrong, but the promise was about the node,
and the identities had already left it. This is invariant 4 inverted: collected
and dropped, where the document said not collected.

The node cannot fix this alone. It holds zero Kubernetes API access by
construction (ADR 0009: no RBAC, default token not mounted), and its only
attribution source is the process cgroup, which yields a pod UID and a container
ID — never a namespace. "This process is in a namespace you allow-listed" is a
claim the node has no way to evaluate.

## Decision

**1. The node asks the controller for its scan scope.** A new node→controller
endpoint, `/v1/node-scope`, takes the querying node's name and answers with the
UIDs of the pods on that node that passed the customer's filters. It sits on the
same port, the same projected-token audience, and the same NetworkPolicy as the
inventory, profile and targets endpoints, and it is node-initiated like all of
them.

The answer is `PodWatcher`'s admitted index, which is exactly the set the
namespace filters and opt-out annotations produce: an excluded pod never enters
that index, and a pod that opts out mid-flight is dropped from it on the next
update. The controller cannot name a pod the filters exclude, because it is
answering from the filtered set rather than applying a filter at answer time.

**2. The node fails closed.** With no scope endpoint configured, or a controller
that cannot be reached, the node scans nothing: `nodescan.Scope`'s zero value
admits nothing, and `Scanner.Scan` requires a scope argument so no call site can
scan unscoped by omission. A lost pass costs nothing — the next one re-scans, and
the controller rebuilds inventory from re-scans (loss-harmless, ADR 0003).
Failing open would mean a controller outage silently reverting the agent to
scanning the whole node, which is the exact behavior this ADR removes.

**3. The cgroup is read before the executable.** Scope is evaluated on the pod
UID first; a process outside the scope has its executable never opened and its
module path never extracted. Nothing about it is collected, rather than collected
and dropped (CLAUDE.md invariant 4). Only an aggregate `filtered_scope` counter
moves — never an identity (invariant 6).

A consequence worth stating: a process belonging to no pod at all — the kubelet,
a systemd-managed daemon, anything outside the kubepods hierarchy — is never in
scope, because it is in no namespace and so no namespace filter can permit it.
Such processes were previously reported with an empty pod UID and discarded by
the controller at the join.

**4. This is not a control channel.** Invariant 1 (ADR 0001) is intact, for the
same reasons as the targets query of ADR 0011 §3. The external backend is not
involved at any point in this exchange. The reply is pod identifiers derived from
the operator's own Helm-values filters applied to the live cluster — never
configuration, never a command. And the reply can only *narrow* what the node
does: a rogue or broken controller can withhold pods, and the node then scans
less; it cannot name a pod the filters exclude, and it cannot make the node scan
anything it would not otherwise scan.

## Consequences

**Easier.** §10.2 is now true as written, and testable: the scope is the
controller's admitted index, so the scanner's namespace behavior is the same
behavior the collector's filters already have, rather than a second
implementation that could drift. Everything built on the node plane inherits it.

**Harder / given up.** The node role no longer works standalone: without a
controller it scans nothing, and its log-only mode is now counters-only. That is
the honest consequence of the promise — a node with no way to know the filters
has no business reading customer binaries — but it does mean the node e2e that
asserted "the scanner finds this Go workload" could no longer pass as written. It
became the fail-closed e2e instead, proving the security property against the
real image and manifest; the positive path is the full-path inventory e2e, which
deploys a controller and asserts on the payload rather than the log.

A pod that starts between two scope queries is scanned one pass later — the
scope is refreshed per pass, and a newly created pod simply is not in the
previous answer. Inventory is a superseding snapshot, so the delay costs nothing
beyond latency.

**Not changed.** The node still holds zero API access, still mounts no default
token, and still reads only `/proc`. The one thing that changed is that it now
knows which of the pods it can see it is permitted to look inside.

This ADR records decisions already implemented in the same pull request, per the
process in [`README.md`](README.md).
