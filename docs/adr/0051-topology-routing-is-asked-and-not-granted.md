# 0051. Topology-aware routing is a request the cluster can silently decline

Date: 2026-08-29

Status: Accepted

Amends: 0032 §1

Adds `spec.trafficDistribution`, the topology-mode annotation, and per-zone
endpoint counts to the Services already carried by `workload_policy`. Adds one
resource to the ClusterRole (`discovery.k8s.io/endpointslices`) and one case to
the informer transform of ADR 0046.

## Context

A customer who wants traffic to stay inside an availability zone sets one field
and is told nothing further. `spec.trafficDistribution: PreferSameZone` is a
request: the EndpointSlice controller decides whether to act on it, and it
declines whenever the endpoints are distributed too unevenly against the zones'
CPU capacity — a Service with four replicas in one zone and one in another gets
no hints at all. There is no event, no condition, and no status field. The
`kubectl get svc` output of a Service whose routing works and one whose routing
was refused is identical.

The cost of the refusal is a cross-zone data transfer bill that the customer
believes they have already eliminated. It is also entirely invisible to every
tool that reads Services, because the answer is not on the Service.

The agent already reports Services (ADR 0048 §4) — name, type, headless, the two
traffic policies. It reports none of what says whether the routing the customer
asked for exists.

`workload_metadata.pod.nodes` gives replica placement per node, and
`node_metadata` gives each node's zone, so a determined backend can already
compute where the replicas are. It cannot compute what the EndpointSlice
controller decided, which is the half that turns a placement observation into a
finding.

## Decision

**1. Endpoint zones are counted, and nothing that identifies an endpoint is
read.** The counts are ready endpoints per zone, per Service, taken from
`endpoints[].zone`. `addresses` (pod IPs), `targetRef` (a pod's name and UID),
`nodeName`, `hostname` and the deprecated topology map are removed in the
informer transform, so they never enter the cache — "not collected" rather than
"collected and dropped" (CLAUDE.md invariant 4, ADR 0046). `targetRef` is not
read at all, which is what keeps this clear of invariant 6: there is no pod
whose identity could leak, because the join to a pod is never made.

The counts are a property of the Service's routing, in the class of the
node-level totals that are collected regardless of workload filters. They are
attached only to Services already resolved to an admitted workload, so a Service
selecting nothing the agent collects is still never named.

This also bounds memory. A Service with thousands of endpoints holds thousands
of addresses; the agent would have carried them for the life of the process to
count something none of them answers.

**2. One entry per address family, never summed.** A dual-stack Service lists
each pod once in an IPv4 slice and once in an IPv6 slice. Merging them into one
zone map doubles every zone — uniformly, so the skew still looks right while
every absolute number is wrong. The payload carries `endpoints[]` keyed by
`address_type`, and the obligation not to sum across them is stated in
`backend-requirements.md`.

**3. Both the field and the annotation are kept verbatim, bounded, not
normalized.** The field's own vocabulary is in motion — `PreferClose` is
deprecated in favour of `PreferSameZone`, and `PreferSameNode` joined them — so a
closed set in the agent would be a list to chase across a fleet the agent cannot
update. Passing the value through costs nothing and dates correctly.

The same holds for the two annotations that asked for this before the field
existed and are still set on live clusters. A value outside Kubernetes'
vocabulary — `auto` where `Auto` was meant, `enabled` where nothing of the sort
exists — configures nothing while looking configured, and that is the same
finding as a declined hint arriving by a different route. Normalizing to a
closed vocabulary would erase it. Both are bounded at 32 characters and dropped
whole past that, the rule ADR 0019 set for build settings.

**4. An unset ready condition counts as ready.** `conditions.ready` is a
pointer, and the API instructs a consumer that does not understand the condition
to treat the endpoint as ready. Reading `nil` as not-ready would under-count
every zone in the cluster, quietly and everywhere.

**5. A Service with no slices carries no endpoint block.** Not a block of zeros:
zero ready endpoints is a claim about a Service, and "the agent has not seen
this Service's slices" is a claim about the agent. The distinction is ADR 0048
§2's, and here it matters because a new informer's cache fills after the first
metadata flush.

## Consequences

**Easier.** The finding is one comparison on one payload: a set
`traffic_distribution` or `topology_mode` against `hinted: 0`. It needs no
history, no profiling and no second read, so it belongs to the first-hour report
rather than to week two. The zone counts also answer, for the first time, where
a Service's traffic can actually land — which the placement fields describe for
pods and never for routes.

**Harder / given up.** One more resource in the ClusterRole, and it is the first
in the `discovery.k8s.io` group. EndpointSlices are also the most numerous
object class the agent now watches — a large cluster has more of them than pods —
so the transform is doing real work on every event, not only guarding a promise.

The counts describe every ready endpoint of the Service, including ones backed
by pods that opted out of collection individually. That is deliberate: routing
does not respect the agent's filters, and a zone distribution computed over the
admitted subset would be a distribution of nothing real. No such pod is named or
counted separately; the number is an aggregate over the Service.

**Not changed.** No new payload kind and no registry row: these are fields on
`workload_policy`, which stays structural, superseding, one per cluster. The
degradation path is the existing one — without the grant the cache never syncs,
`endpoint_slices` appears in `unavailable_sources`, and the Services still ship
without the block (ADR 0033).

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
