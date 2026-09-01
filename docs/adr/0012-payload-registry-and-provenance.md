# 0012. Payload registry, provenance discriminator, and metadata delivery

Date: 2026-08-20
Status: Accepted
Amended by: 0013, 0014, 0017, 0020, 0021, 0022, 0027, 0054, 0064

## Context

The agent had accumulated four payload kinds (`usage_snapshot`, `usage_window`,
`oom_kill`, `go_inventory`) plus `ebpf_profile`, each introduced by the slice
that needed it. Three problems had grown quietly around them.

**Collected but never shipped.** `collector.PodInfo` (requests, limits, QoS,
declared ports, image digest) and `collector.NodeInfo` (size, instance type,
capacity type) had been collected since the first collector slices and reached
no payload — only the log. The backend therefore received usage numbers with
nothing to compare them against: a rollup says a container burned 15 core-seconds,
and nothing in the protocol says what it requested. Every finding that compares
observation to declaration was unbuildable, not for want of collection, but for
want of delivery.

**No placement key.** Zone and region were read nowhere in the repository. A
usage record is keyed by (namespace, workload, container) and merged across
replicas and nodes (ADR 0006), so nothing that left the cluster could be
attributed to a failure domain — or to the line of a cloud bill that a failure
domain determines.

**Provenance was implicit.** A number read from a spec, a number the kubelet
measured, and a number a profiler sampled are different kinds of claim with
different failure modes, and nothing in the payload distinguished them. A
backend that merges them under one natural key produces a confident wrong answer,
and every golden test stays green while it does — the defect surfaces in a
customer report, not in CI.

All three are cheap to fix while there are five payload kinds and expensive
later: adding a discriminator to a shipped protocol means migrating every
consumer of every kind.

## Decision

**1. The payload registry is a fixed list, recorded here.** Every payload kind
has one natural key, one delivery discipline, and one provenance class:

| Kind | Natural key | Delivery | Provenance |
|---|---|---|---|
| `usage_snapshot` | (window start, window length) | supersedes its predecessor | measured |
| `usage_window` | (window start, window length) | final, supersedes snapshots | measured |
| `oom_kill` | (finished-at, namespace, pod, container, restart count) | accumulates | journal |
| `go_inventory` | the kind itself (one per cluster) | supersedes | structural |
| `workload_metadata` | the kind itself (one per cluster) | supersedes | structural |
| `node_metadata` | the kind itself (one per cluster) | supersedes | structural |
| `ebpf_profile` | (namespace, workload, container, capture start–end) | accumulates | sampled |

A new kind is added by extending this table in a superseding ADR, not by
inventing a `kind` string at a call site.

**2. Provenance is a field, named `source`, on the payload envelope.** Four
classes, which the backend must never merge under one key:

- `structural` — read from an object's spec or status. Declared, deterministic,
  independent of sampling.
- `measured` — obtained by polling an instrument (the kubelet), and therefore
  subject to scrape failure.
- `journal` — derived from object metadata that records history (conditions,
  restart counts, terminations).
- `sampled` — statistical (profiles).

The discriminator sits on the payload, not on each record, because a kind has
exactly one provenance.

**3. Metadata is a snapshot, not a stream, and carries no window.** Workload and
node metadata describe current cluster state, so each flush replaces its
predecessor under a fixed key, exactly as `go_inventory` does. They are derived
on each flush from the watchers' live indexes and hold no state of their own,
which keeps them loss-harmless by construction (ADR 0003). A spec has no window;
the payload is true as of the flush that wrote it, ordered by its sequence.

**4. Join keys.** `rollup.Key` stays frozen as the container-scoped upsert key
(ADR 0006); nothing is added to it. A `workload_metadata` record's key is that
same four-field key **plus the image digest**, because a rollout runs two builds
at once and their declared resources may differ — keying on the digest reports
both truthfully instead of letting whichever pod was observed last overwrite the
other. The extra records collapse back to one when the rollout finishes.

Placement is joined by node name: a workload-metadata record counts its replicas
per node, and `node_metadata` maps a node name to its zone, region, instance type
and size. Zone therefore lives in exactly one place, and pod-scoped facts are
never forced into the container-scoped rollup key.

**5. Replica counts are reported, never interpreted.** A record carries
`replicas` with two breakdowns that the agent does not reduce further: `phases`
(pod phase → count) and `nodes` (node name → count). Unscheduled pods have no
node, so `nodes` may sum to fewer than `replicas`; that shortfall is the
pending-scheduling signal and is left for the backend to read as one. The agent
applies no threshold and draws no conclusion (ADR 0004's division of labor).

## Consequences

**Easier.** The backend can join a usage rollup to the envelope its workload
declared, and either to the zone and instance type it ran on. Adding a sensor is
now a table entry plus a writer, with the provenance question already answered.
A rollout is visible as two records rather than as a flapping one.

**Harder / given up.** Two more superseding files in the spool, bounded by the
same `maxAge` sweep. Per-digest keying means a workload's record count doubles
for the duration of a rollout — bounded, self-healing, and preferable to silent
overwrite. Metadata is flushed on the coverage cadence, so it lags a change by up
to that interval; it is a snapshot of current state, never a change log, and a
workload created and destroyed between two flushes is never reported.

**Known gap, recorded rather than hidden.** The `source` field ships today only
on `workload_metadata`, `node_metadata` and `ebpf_profile`. `usage_snapshot`,
`usage_window`, `oom_kill` and `go_inventory` do not yet carry it: adding it to
them changes their golden bytes, which is a protocol change with its own
migration, and it belongs with the related work of making observation
completeness explicit (an unsuccessful kubelet scrape is currently
indistinguishable from a genuine zero — see `internal/rollup.CPUUsage`). The
`measured` and `journal` class names are fixed here so that work adds a field
rather than reopening this decision.

This ADR records decisions already implemented in the same pull request. It
describes what the code does, and the golden payload files under
`internal/sink/testdata/` are its executable form.
