# 0014. Facts of a different scope than the record's key are nested, not flattened

Date: 2026-08-20
Status: Accepted
Amends: 0012 §5
Amended by: 0053

Amends §5 of [ADR 0012](0012-payload-registry-and-provenance.md). That ADR is
not superseded: its registry, provenance classes, and join keys stand
unchanged. Only the shape in which a record carries pod-scoped facts changes.

## Context

`workload_metadata` records are keyed per container:
`(namespace, workload_kind, workload_name, container, image_digest)`. That is
the right key — requests, limits, ports and CFS quota are all declared and
enforced per container, and the usage rollups it joins to use the same
container-scoped key (ADR 0006).

But three of a record's fields were never container-scoped. `replicas`,
`phases` and `nodes` describe the *pods* carrying the container, and `qos_class`
is assigned by the kubelet from the whole pod. Flattened onto the record, they
were repeated across every container of a pod: a meshed pod with `app` and
`istio-proxy` produced two records each reading `replicas: 2`, and a consumer
grouping records by workload and summing that field got four replicas. The
result is plausible, wrong, and produces no failing test anywhere — the same
class of defect the provenance discriminator exists to prevent, at a different
level.

The shape was noticed one pull request after it shipped, before any consumer
existed. Every later payload faces the same question: network counters are
pod-scoped (a shared netns), PDB and PriorityClass facts are workload-scoped,
and all of them will want to travel next to container-scoped records.

## Decision

**A fact whose scope differs from the record's key lives in a named object for
that scope, never flattened onto the record.** In `workload_metadata` that is a
`pod` object holding `qos_class`, `replicas`, `phases` and `nodes`.

The rule is about scope, not about arithmetic. `qos_class` cannot be summed and
still belongs in `pod`, because the question a reader must be able to answer
from the payload alone is *what is this a fact about*, not *may I add it up*.

Documenting the trap in the backend contract was considered and rejected. A
payload whose correct use depends on prose is a payload that will be misused:
prose is not present at the moment someone writes a `GROUP BY`. Nesting makes
the mistake self-evident at that moment — summing `pod.replicas` across the
containers of one pod is visibly meaningless in a way summing `replicas` is not.

## Consequences

**Easier.** Later payloads have a shape to follow rather than a judgment call:
pod-scoped network counters get a `pod` object, workload-scoped disruption facts
get a `workload` one. A reader can determine a field's subject from the payload
without consulting an ADR.

**Harder / given up.** The `workload_metadata` golden changed one pull request
after it was introduced. That is the cost of having shipped the flat shape; it
is paid once, and at the cheapest moment it will ever be available — no consumer
has read this payload yet. The pod block is still repeated per container, so the
payload carries redundant bytes; the alternative — a separate pod-scoped payload
kind — was rejected in ADR 0012 §4 because a pod's build identity is the tuple
of its containers' image digests, which would need a synthetic key of its own.

**Not changed.** The container-scoped record key itself. Splitting per container
is what makes the sidecar tax and the request-versus-usage comparison expressible
at all, and it stays reversible in the only direction that matters: a backend can
always sum container records into a pod or workload total, and can never split a
pod total back into containers.

This ADR records a decision already implemented in the same pull request, per the
process in [`README.md`](README.md).
