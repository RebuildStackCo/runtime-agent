# 0006. Usage collection: kubelet counters, mergeable windows, snapshot delivery

Date: 2026-08-06
Status: Accepted
Amended by: 0007, 0013, 0016, 0027, 0045, 0052, 0053

## Context

The contract (`backend-requirements.md` §4) requires mergeable usage rollups
per workload and container and forbids raw time series (§9). ADR 0001 removes
the option of on-demand freshness: the backend cannot ask the agent to "stream
faster while the customer watches", so whatever freshness the UI has must be a
standing agent behavior. At the same time the product's recommendations must
be numerically defensible — a histogram alone, sampled at some cadence, is not
enough for signals that have exact sources.

Candidate sources for usage data:

- **metrics-server** — an external dependency that may not be installed,
  keeps no history, and smooths away peaks.
- **Prometheus** — a side channel (`security.md` §10.1) that is present in
  most but not all clusters, with customer-controlled retention and tenancy.
- **kubelet endpoints** — always present, no dependency. `/stats/summary`
  carries a cumulative CPU counter (`usageCoreNanoSeconds`), the memory
  working set gauge, and — behind the `KubeletPSI` feature gate on cgroup v2
  nodes (alpha in 1.33) — PSI stall-time counters. `/metrics/cadvisor`
  carries the CFS throttling counters. The kubelet refreshes container stats
  on a ~10 s internal cadence, so polling faster gains nothing.

Two physical limits shape the design. Cumulative counters make totals exact
regardless of poll cadence — a missed poll widens the next delta but loses
nothing. Gauges do not: the true memory peak between two samples is
unobservable on cgroup v2 (the kernel max-usage counter is a cgroup v1
artifact), so peak risk must come from other signals (OOM events, PSI), not
from faster sampling.

Customer clusters run a spread of Kubernetes versions; a design that requires
the newest kubelet would shrink the installable base, and one that assumes
the oldest would discard the strongest signals.

## Decision

1. **Source.** The controller polls every node's kubelet through the API
   server proxy (`get nodes/proxy`): `GET /stats/summary` and
   `GET /metrics/cadvisor`, every 30 seconds. No metrics-server or Prometheus
   dependency on this path.
2. **Signals per container.** Exact window totals from cumulative counters:
   CPU core-seconds, throttled and total CFS periods, PSI stall time where
   exposed. Sampled gauge: memory working set. Counter deltas are computed
   from the timestamps inside the kubelet response, never from scrape time;
   a re-served snapshot with an unchanged timestamp is discarded. A negative
   delta means container restart: the new value becomes the baseline and no
   sample is emitted. A container's first observation is counted as a delta
   from container start, so anything that survives to one poll is attributed;
   containers that live and die between polls are lost and acknowledged as
   such.
3. **Runtime signal probing.** The agent detects which signals each kubelet
   actually exposes, collects what is present, and reports the active signal
   set in its self-info. A missing signal degrades the record; it never fails
   collection.
4. **Reduction.** Aggregation key: (namespace, workload, container) — all
   replicas of a workload merge into one record. Distributions are histograms
   with fixed logarithmic bucket boundaries, growth factor 2^(1/8) (~9% per
   bucket), anchored at 1 millicore for CPU and 1 MiB for memory, sparsely
   encoded. Alongside: exact min/max/count (and sum for memory). Bucket
   boundaries are frozen — changing them breaks mergeability with history and
   is a payload schema version change, never a silent edit.
5. **Windows.** Rollups accumulate over wall-clock-aligned windows. The
   length is an agent constant — initially one hour — and is part of the
   record's natural key (window start, window length), so it can change
   without a schema break and the backend can merge shorter windows into
   longer ones losslessly (the merge property).
6. **Delivery.** Once a minute the agent ships a snapshot of the still-open
   window. The backend upserts by natural key: a newer snapshot of a window
   supersedes an older one, and the closed-window record supersedes every
   snapshot (`backend-requirements.md` §4). Snapshots are the same aggregate
   record shape — shipping them does not relax the no-raw-series rule.
   Events (OOM kills) bypass windows entirely and ship immediately.
7. **Filtering.** Every sample passes the same pod filter that gates all
   pod-derived signals, before accumulation — an excluded pod's usage is
   never held beyond parsing.

## Consequences

Easier:

- UI freshness of ~1–2 minutes end-to-end (kubelet ~10 s → poll 30 s →
  snapshot 60 s) while only aggregates ever leave the cluster.
- An agent restart loses at most one snapshot interval, not a whole window —
  the last snapshot already reached the backend.
- Window length and snapshot cadence are free knobs: neither touches the
  schema, the merge property covers re-aggregation.
- Recommendations rest on exact totals; histogram error is bounded at ~±4.5%
  on any percentile.

Harder, or given up:

- `get nodes/proxy` technically permits every kubelet GET endpoint through
  the API server, including node logs. The agent calls exactly two stats
  paths; `security.md` must disclose the gap between what the verb allows
  and what the agent does.
- All node stats flow through the API server. Very large clusters will want
  the DaemonSet / direct-kubelet path (narrower `nodes/stats` RBAC, broader
  network access) — a transport swap that leaves the rollup model unchanged.
- Per-replica skew is invisible at the workload-level key: a bimodal
  histogram shows that replicas diverge, not which one.
- Memory peaks between samples are unobservable; OOM events and, where
  available, PSI carry that risk signal instead.
- The backend must implement supersede-by-key ingest, ordered by agent-side
  snapshot sequence rather than arrival order.
- A support floor is now defined (see README): baseline Kubernetes 1.33;
  the full signal set (PSI) requires 1.34+ with cgroup v2 nodes.
