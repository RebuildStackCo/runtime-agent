# 0069. Liveness is the process; readiness is what it has collected

Date: 2026-09-04
Status: Accepted

## Context

Neither role had a probe of any kind — no liveness, no readiness, no startup, on
neither the Deployment nor the DaemonSet. Two failures follow from that, and
both are silent.

**A wedged controller is never restarted.** The agent's periodic pass holds the
locks that every payload is assembled behind. A goroutine that stops returning
leaves a process that is scheduled, holding its port open, answering nothing —
and Kubernetes restarts a container only for exiting or for a failing liveness
probe. There was neither.

**A rollout reports success before collection starts.** A pod with no readiness
probe is `Ready` as soon as its container process exists, so
`kubectl rollout status` returns before the controller has synced one of its 18
informers. The customer's automation is told the agent is up; the agent is
listing.

The port question was already answered by the shape of the install. 8080 exists
only in the two profiles that install a DaemonSet, and it carries the node
reports; a probe on it would make the health of every install depend on a
component half of them do not have, and would put a kubelet's request on the
port whose whole boundary is "only the node role may speak here" (ADR 0040).
The image is `distroless/static` with no shell (ADR 0037), so an `exec` probe
cannot run at all. What is left is an HTTP listener on a port of its own.

And there is a trap in the obvious implementation. The tempting liveness check
is "can I reach the API server" — it is the agent's one dependency and the thing
most likely to be wrong. It is also the one check that must never be made: the
DaemonSet runs on every node, so an API server outage would fail that probe on
every node at once and the kubelets would restart the whole fleet together,
against an API server that is already failing. An agent that is a guest of
somebody else's cluster must not become an amplifier of their outage.

## Decision

**1. One always-on HTTP listener per role, on port 9090, answering `/livez` and
`/readyz`.** Independent of 8080 and rendered in every profile, because the two
questions a kubelet asks are not a feature of an installation option. Both paths
are read-only: only GET and HEAD are answered, no request body is read, no query
is parsed, and no reply names anything read from the cluster — a resource class
or a component at most (invariants 1 and 6). It is on no Service: probes reach
the pod directly, and a Service entry would give every pod in the cluster a
stable name for it.

9090 rather than a number beside 8080, because the metrics endpoint of a later
slice joins *this* listener as a path. One port means one container port, one
probe target and one hole in the network policy, now and after metrics land.

**2. Liveness is each role's own loop, and depends on nothing outside the
process.** Each role stamps a heartbeat at the top of every pass — the
controller's periodic flush, the node's scan — and `/livez` is 503 when the
stamp is stale. It reads no cache, opens no socket and asks nobody. So an API
server outage does not restart a controller, and a controller outage does not
restart the DaemonSet on every node; the agent stays up, collects what it still
can, and says what it could not in `collection_coverage` (ADR 0054).

The stamp is taken at the start of a pass and not at the end. A pass that hangs
half way through then goes stale, which is precisely the state this exists to
catch; stamping at the end would let a wedged pass look like a pass that has not
finished yet, forever.

The deadlines are multiples of each loop's own period, so several consecutive
passes must be missed: three intervals for the controller, and for the node
three intervals plus the minute one pass may spend waiting out the two 30-second
client timeouts against a controller that is not answering.

**3. Readiness is the caches that gate collection, and the spool.** For the
controller: the pod index, the owner chain, namespaces and nodes are synced, and
a spool is open. The nine policy caches are deliberately **not** among them —
ADR 0033 decided that a permission the agent was not given degrades one payload
and stops nothing, so waiting for them here would turn a documented degradation
into a pod that is never `Ready` and a `rollout status` that never returns. What
those caches are doing is already reported, per source, in the coverage payload.

For the node: readiness is that the first scan pass has finished. Not that the
scope query succeeded — the node fails closed when the controller is unreachable
(ADR 0015), and tying readiness to that would flip every node pod to `NotReady`
whenever the controller is upgraded. That is §2's amplifier one layer down. A
node whose controller is down has finished a pass that scanned nothing, and the
size of that silence is a coverage counter, not a health state.

Readiness latches, as `HasSynced` does. A cache that later stops being fed is
ADR 0035's business and its answer is to stop the agent, which ends readiness by
ending the pod.

**4. The address reaches each role the way that role is configured.** The
controller reads `health.listenAddress` from its configuration file, beside the
other port it opens; the node takes a `-health-address` flag. The asymmetry is
not aesthetic: the node's file is rendered only by the profile that profiles, so
putting the address there would mean a ConfigMap, a mount and a checksum
annotation in the `inventory` profile to carry one string — and the node's
schema is deliberately narrow (ADR 0025), while its operational wiring is
already flags. Empty means no listener in both roles: the chart is the only
installer (ADR 0036) and it always renders one, so an agent run by hand opens
nothing it was not asked to.

**5. The network policy gains a second ingress rule, on the health port, with no
source.** The policy selects the controller pod, and a selected pod denies
everything not permitted — so without this the probes would be denied on any CNI
that enforces the policy, and the controller would be restarted forever by a
liveness probe that could not reach it. The rule names no source because the
caller is the kubelet, arriving from the node's own address: not a pod, and not
something a `podSelector` or a portable `ipBlock` in a template can name. What
the rule opens is two paths that answer yes or no about one process.

**6. The startup probe exists so liveness does not fire during the first sync,
and its budget is ten minutes.** Syncing 18 cluster-wide caches on a large
cluster takes tens of seconds, and `WaitForCacheSync` has no timeout at all. Ten
minutes is deliberately longer than the five a gating cache may fail before the
agent stops itself (ADR 0035): a refused LIST should produce the agent's own
error, naming the resource and the grant, rather than an anonymous probe
restart. The node's budget is three minutes, which covers a first pass that
waits out both client timeouts against a controller that is not up yet.

## Consequences

**Easier.** A controller that stops making progress is restarted within about
three and a half minutes instead of never. `kubectl rollout status` now means
what it says: the controller is `Ready` when it holds the cluster's state, and
the DaemonSet has rolled out when every node has actually completed a scan.
`internal/chartrender/chart_test.go` asserts that every pod in every profile
carries all three probes, so a workload added later without them fails CI.

**Harder / given up.** Both roles now open a port that was not open before, in
every profile — including `metrics-only`, which until now opened none. It is
stated in `docs/security.md` §5, it carries no collected data, and it is
reachable only from within the cluster; but "the controller-only install opens
no port" was a true sentence and is not any more.

The second policy rule is unrestricted by source. On a CNI that enforces
policies, any pod in the cluster can now ask this port whether the agent is
alive and whether its caches are synced. That is the whole of what it can learn:
two booleans about the agent, and nothing about the cluster the agent watches.
The alternative — naming the node address — is not expressible in a template
that does not know the cluster's node CIDR, and a policy that silently selects
nothing would be worse than one that admits a question with no answer worth
having.

Liveness cannot detect a controller that is running its loop and collecting
nothing, because that is not what liveness is for. That failure has its own
mechanisms already: the watchdog that stops the agent when a gating cache stays
broken (ADR 0035), and the coverage counters that size what was not seen
(ADR 0054).

**Not changed.** Nothing about what the agent collects, stores or transmits. No
payload kind, no field, no grant, no capability, no mount; `internal/sink/registry.go`
is untouched. `docs/security.md` gains one row because the agent opens a port,
not because it learns anything new.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
