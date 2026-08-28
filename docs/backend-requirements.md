# Backend Requirements — the Agent's Perspective

What `runtime-agent` needs from the backend in order to operate. This is a
contract document, not a design: it states behavior and guarantees, not
endpoints, schemas, or implementation. Anything the backend does beyond this
contract (analysis, cost model, findings, reports, read API, MCP) is a product
concern and out of scope here — with one exception: nothing the backend does
may violate the principles in §1.

Requirement levels follow RFC 2119 (**MUST** / **SHOULD** / **MUST NOT**).

---

## 1. Principles the backend MUST preserve

1. **One-way flow.** The agent initiates every connection. The backend
   MUST NOT send commands, tasks, or configuration to the agent. Responses
   to agent requests carry only protocol outcomes (acknowledgments, errors,
   issued certificates). If a response contains anything else, the agent
   ignores it by design.
2. **Agent autonomy.** The agent must remain fully functional for collection
   with the backend unreachable. Nothing in the contract may require a
   round-trip to the backend for the agent to decide what to collect.
3. **The agent is replaceable, the data is not.** The backend is the
   long-term source of truth for everything the agent ships; the agent keeps
   only a working window. Loss of an agent's local state MUST be recoverable
   through this contract alone (re-enroll + continued ingest), with no data
   history lost on the backend side.

---

## 2. Identity and enrollment

The full lifecycle is described in `security.md` §6; the backend obligations:

- **Cluster records exist before agents.** The backend MUST allow creating a
  cluster record (name, labels, org) ahead of any agent contact, and MUST
  treat an enrollment against a non-existent record as an error — never
  auto-create clusters from enrollment.
- **Enrollment.** The backend MUST accept a certificate signing request
  authenticated by a per-cluster enrollment token and return a client
  certificate carrying the cluster identity. Tokens MUST be short-lived and
  MUST remain reusable within their TTL (agents restart during rollout).
- **Fingerprint check.** Enrollment includes a cluster fingerprint (UID of
  `kube-system`). The backend MUST reject an enrollment whose fingerprint
  contradicts the one already bound to the cluster record, and MUST surface
  the conflict to the operator rather than silently mixing data from two
  clusters.
- **Renewal.** The backend MUST issue a renewed certificate over the existing
  mTLS session before expiry, without any operator involvement.
- **Re-enroll.** For an existing cluster record, the operator MUST be able to
  issue a fresh enrollment token; a successful re-enroll MUST revoke the
  previous certificate and MUST keep the cluster's data history continuous
  (same cluster identity).
- **Revocation.** The backend MUST be able to revoke any cluster certificate
  at any time, effective on the next connection attempt.

## 3. Transport

- Single fixed domain, mTLS, certificates issued by the backend's private CA.
  The agent pins that CA; the backend MUST NOT rely on public PKI for its
  server identity and MUST keep the CA stable across infrastructure changes
  (rotations follow a published overlap window so pinned agents never break).
- The domain and its addressing MUST be stable enough for enterprise egress
  allowlists. Changing the domain is a breaking change to every installed
  agent and requires a migration plan.
- **Wire format: protobuf payloads over plain HTTPS POST** (Connect-style
  RPC is acceptable; bare gRPC is not part of the contract). The data plane
  MUST work over HTTP/1.1 through enterprise proxies — no dependency on
  HTTP/2-only features such as gRPC trailers. Rationale: the traffic is
  batch upload, not streaming, and corporate egress infrastructure is the
  environment this system lives in.
- Enrollment endpoints MUST be reachable over plain HTTPS (server-auth TLS
  only) — by definition they are used before a client certificate exists.

## 4. Ingest

### What arrives

The agent ships, per cluster:

- mergeable usage rollups per workload and container over wall-clock-aligned
  windows (initially hourly; the window length is part of the record key):
  fixed-boundary histograms for CPU and memory working set, plus exact
  window totals — CPU core-seconds, CFS throttling periods, and PSI stall
  time where the cluster exposes it (ADR 0006)
- workload and node metadata: names, namespaces, owner chains, image
  digests, Go version, module path, declared requests/limits, declared
  container ports, and — in a per-record `pod` block — QoS class and replica
  counts per pod phase and per node; plus instance types, capacity type,
  allocatable, and node zone/region. These arrive as superseding snapshots of
  current state, not as a change log (ADR 0012)
- OOM and restart events, plus the restart counter as it stands, which carries
  the restarts that predate the agent and belong to no window (ADR 0034)
- symbolized, allow-list-filtered profiles, keyed to image digest
- a coverage report: **aggregate counts per filter type** (discovered /
  collected / excluded by namespace filter / by annotation / …). Identities
  of excluded objects — names, namespaces, deny-listed paths — are never
  transmitted; the backend MUST NOT expect or request them
- a fingerprint (hash + last-change timestamp) of the effective filter
  configuration, carrying no configuration content. Every upload is
  attributable to the configuration that produced it, so stability checks
  can distinguish "the data changed" from "the filters changed"
- agent self-info: version, install profile, and effective RBAC summary, so
  every report can state what its findings do and do not rest on. The active
  usage signal set (which kubelet signals — e.g. PSI — this cluster exposes)
  travels in each measured payload's `observation` block rather than here, so
  it is attached to the data it qualifies (ADR 0013)

All payloads are denominated in **resource units** (core-hours, GiB-hours,
counts, ratios). The protocol has no monetary fields: the agent knows nothing
about prices or invoices, and pricing is applied backend-side at render time
from a versioned pricing snapshot.

Every payload declares a `kind`, and carries its **provenance** in a `source`
field: `structural` (read from a spec or status), `measured` (polled from an
instrument), `journal` (derived from object history), or `sampled`
(statistical). The
backend MUST NOT merge payloads of different provenance under one natural key:
a declared value and a measured one are different kinds of claim, and
collapsing them yields a confident wrong answer with no failing test to show
for it. The registry of kinds, their natural keys, and their delivery
discipline is [ADR 0012](adr/0012-payload-registry-and-provenance.md).

**Records are container-scoped; a differently scoped fact is nested**
([ADR 0014](adr/0014-scoped-facts-are-nested.md)). Usage rollups and workload
metadata are keyed per container, because requests, limits and CFS quota are
declared and enforced per container. A pod with an injected sidecar therefore
appears as several records sharing a workload and differing in `container`.
Summing their totals gives the pod or workload total — that is the intended use.
Facts that belong to the pod rather than the container are not repeated as
top-level fields but nested in a `pod` object, so a reader can tell from the
payload alone what a field is a fact about.

**`pod.phases` is placement and lifecycle, never health.** The counts are
Kubernetes pod phases verbatim, and the backend MUST NOT read `Running` as
"healthy": a pod in CrashLoopBackOff is `Running`, and so is one whose readiness
probe has never passed. Readiness and restart counts are not in this payload.
`Succeeded` and `Failed` pods are reported too — a CronJob's workload
legitimately shows dozens of them consuming nothing — so `pod.replicas` should
be read together with its breakdown rather than alone; the agent reports the
phases and does not decide which of them count.

**Measured payloads state what was observed, and the backend MUST use it**
([ADR 0013](adr/0013-observation-completeness.md)). Each usage payload carries an
`observation` block — poll cadence, cumulative counts of kubelet requests
attempted and failed, and the signal set this cluster exposes — and each record
carries its own completeness:

- `cpu.covered_nanoseconds` — how much of the window this container was actually
  observed for. Against `window_seconds` it bounds what the record can support.
- `cpu.throttling_samples`, `cpu.psi_samples`, `memory.psi_samples` — how many
  observations carried each signal. **A zero total with a zero sample count means
  "never observed", not "zero".** A backend that reads the first as the second
  will report a throttling-free workload on the strength of a failed scrape.

The counters are cumulative since agent start; the backend takes differences
between deliveries, exactly as it does for every other counter here.

**`container_restarts` counts exactly and explains partially**
([ADR 0020](adr/0020-container-restart-journal.md)). Each record covers one
(namespace, pod, container) within one hour-aligned window, and its three
restart fields are not interchangeable:

- `restarts` is exact. It comes from the restart counter's advance, so it counts
  restarts the agent never saw individually.
- `reasons` is a sample of that total. A container status carries only its most
  recent termination, so restarts that happened between two status updates are
  counted without a reason.
- `reasons_unobserved` is the difference. **`sum(reasons) + reasons_unobserved ==
  restarts` holds for every record**, and a backend that renders the breakdown
  as if it were the whole must not: a container restarting faster than the
  informer delivers updates would appear to restart less than it does.

The reason keys are a fixed set (`OOMKilled`, `Error`, `Completed`,
`ContainerCannotRun`, `ContainerStatusUnknown`, `DeadlineExceeded`,
`StartError`, `Evicted`) plus `other` for anything the container runtime reports
outside it. The backend MUST tolerate `other` and MUST NOT assume the set never
grows.

The payload supersedes by (window start, window length): each flush rewrites the
open window's payload with the window's running totals, and the last write
before the window ends is final. The backend MUST upsert by that key and MUST
NOT add successive deliveries together — they are cumulative within the window,
not increments. Windows before the agent started watching a container are
absent by design: a restart Kubernetes does not timestamp cannot be placed in
one.

`oom_kill` and `container_restarts` describe the same events from two angles and
**do overlap**: an OOM-killed container appears as an `oom_kill` event and
within the window's `reasons["OOMKilled"]`. This is not double counting to be
reconciled away — `restarts` is the container's true total, and the OOM events
carry the per-kill detail (memory limit, exit code, restart count at the time)
that a window aggregate cannot.

**`restart_counters` is a reading, and MUST NOT be added to the windows**
([ADR 0034](adr/0034-restart-counter-readings.md)). It is a superseding snapshot
of the kubelet's restart counter for every collected container that has ever
restarted, keyed by the payload kind and carrying the same `captured_at` as the
workload metadata of that flush.

- `restarts` is the container's total since its pod was created, including
  restarts that happened before the agent was installed. The windows of
  `container_restarts` are a **subset** of it. A backend that sums the two
  reports a cluster restarting roughly twice as often as it does.
- `restarts_before_observation` is how much of that total the agent never
  watched, and `observed_since` is when it started watching. Those restarts are
  in no window and never will be: Kubernetes does not timestamp them.
- **`restarts - restarts_before_observation` is never negative.** Both are read
  from one observation, so the subtraction this section prescribes is always
  defined, and no defensive clamp is needed
  ([ADR 0043](adr/0043-the-counter-reading-comes-from-one-observation.md)).
- `observed_since` and `pod_created_at` bound the interval those restarts are
  spread over, and nothing narrows it further. The backend MAY derive a rate
  over that interval; it MUST NOT place them at a point inside it.
- `container_started_at` is how long the current incarnation has been running,
  and is absent when the container is not running. A large `restarts` with a
  long-running current incarnation is a workload that recovered, which the total
  alone does not distinguish from one still failing.
- `last_termination` is the single restart Kubernetes does date, with the
  reason as the kubelet wrote it. Unlike the window's `reasons` keys this is a
  free value from a set the backend does not control, so it MUST be tolerated
  rather than matched against a fixed list.

A container whose counter is zero has no record: the payload is a snapshot, so
absence means zero rather than unknown. An empty record list means no collected
container has ever restarted, and MUST NOT be read as a failed collection.

**`unavailable_sources` disqualifies the payload's absences, and says nothing
about why** ([ADR 0033](adr/0033-policy-sources-degrade.md),
[ADR 0035](adr/0035-watch-failures-are-noticed.md)). `workload_policy` and
`cluster_policy` each declare the sources they could not read at that capture.
The field names resource classes, is omitted when every source was read, and
each payload declares only its own sources.

- The rule it exists for: in these payloads an absent record is a positive
  statement — "nothing constrains this workload". When a source is listed, that
  statement is void for anything that source would have supplied, including
  workloads that vanish from the payload entirely rather than appearing with a
  field missing. The backend MUST NOT treat the payload as a complete picture of
  what it declares unreadable.
- One list means one thing: not available at this capture. It does **not** say
  whether the permission was never granted, was taken away from the running
  agent, or was lost to a webhook or an unreachable API server, and the backend
  MUST NOT infer a cause from it.
- It is about that capture and no other. A source listed once and not again was
  readable again; a source that is readable again may still be listed for a
  capture or two, because the agent errs toward declaring a gap. Neither is a
  claim that the source is permanently gone, and the backend MUST NOT latch it.
- A missing grant the agent cannot work without produces no payload at all — the
  agent exits (ADR 0035). Absence of deliveries and a declared source are
  therefore different signals, and the backend SHOULD alert on the first through
  whatever it uses for a silent cluster.

**`pod_disruptions` is what the cluster took away, not what failed**
([ADR 0021](adr/0021-pod-lifecycle-journal.md)). One record per pod per
hour-aligned window: preempted, evicted under node pressure, drained by a taint,
or removed through the eviction API. A pod whose own container exited non-zero
is **not** here — that is `container_restarts` and `oom_kill`. The backend MUST
NOT read an absence of disruption records as a healthy cluster; it means no pod
was taken away, which is the normal state.

- `disrupted_at` is Kubernetes' own timestamp, not the observation instant, so a
  disruption that happened before the agent started still lands in the window
  where it occurred. Records may therefore arrive for a window already
  delivered; the payload supersedes by (window start, window length), so the
  backend MUST upsert and MUST NOT append.
- Records are keyed by (namespace, pod) within a window and are idempotent: the
  same pod reported again after an agent restart rewrites its record rather than
  duplicating it.
- `node` is the node the pod was taken from. For a node-pressure eviction it
  names the node that was under pressure — joined against `node_metadata` it is
  what turns "a pod was evicted" into "this instance type ran out".
- `reason` is drawn from a fixed set (`TerminationByKubelet`,
  `PreemptionByScheduler`, `DeletionByTaintManager`, `DeletionByPodGC`,
  `EvictionByEvictionAPI`, `Evicted`) plus `other`. The backend MUST tolerate
  `other` and MUST NOT assume the set never grows.

**`workload_metadata.workload` and `.pod` are two different scopes, and neither
is per record** (ADR 0014). A pod's containers each produce a record repeating
both blocks, so **the backend MUST NOT sum either across one workload's
records**. `rollout` is absent for a kind that replaces no replicas — a Job, a
CronJob, a bare ReplicaSet, an owner-less pod — and MUST NOT be rendered as one
that replaces everything at once. A `rollout` carrying only `unread: true` is a
workload managed by a custom resource the agent holds no RBAC for, such as an
Argo Rollout: **unobserved, not absent**, and MUST NOT be read as no strategy.

**`workload_metadata.pod.unscheduled` explains the replica shortfall, it does
not restate it.** The gap between `replicas` and the sum of `nodes` has always
shown how many replicas are unplaced; `unscheduled` counts those by the
scheduler's reason. **The backend MUST NOT treat these as two independent
counts** — they describe the same pods, and summing or cross-checking them as
separate signals will produce nonsense during the moments when a pod is between
states.

The reasons are not interchangeable and MUST NOT be merged: `Unschedulable`
means nothing in the cluster fits the pod and is a capacity fact;
`SchedulingGated` means the pod is deliberately held by a scheduling gate and is
**not** waiting on capacity; `SchedulerError` is the scheduler failing on the
pod's own spec. Rendering a gated pod as a capacity shortage invents a shortage
that does not exist.

**Structural snapshots say when they were taken, and the Go inventory says how
complete it is** ([ADR 0017](adr/0017-build-facts-keyed-by-digest.md)).
`go_inventory`, `workload_metadata` and `node_metadata` each carry `captured_at`
— the instant the snapshot was assembled, which is not a window and not the age
of the facts inside it. `go_inventory` additionally carries a `coverage` block
whose counters are cumulative from the `since` instant carried with them:

- `nodes_reported` — how many nodes *currently in the cluster* have delivered at
  least one scan; a node that has left is not counted. Read against the node
  count in `node_metadata`, a shortfall means part of the fleet is not
  reporting, and the inventory is incomplete for reasons no record can express.
- `facts_received` / `facts_joined` / `facts_unjoined` / `facts_undigested` —
  what arrived and what could not be attributed. **The backend MUST NOT read a
  missing workload as "not a Go workload"** when these say otherwise.

**A `go_inventory` record exists only while its workload does**
([ADR 0018](adr/0018-inventory-records-live-only-while-their-workload-does.md)).
The agent drops records whose workload container has left its filtered pod index
— deleted, or opted out by annotation — so a record's disappearance between two
snapshots means "no longer collected", not "not observed this time". The backend
MUST treat a superseding snapshot as the complete current truth and MUST NOT
carry forward records absent from it. A short-lived workload (a CronJob's pods)
may legitimately appear in one snapshot and not the next; accumulating "has ever
been a Go workload" across snapshots is the backend's job, and `captured_at` is
what dates them.

**`go_build` is write-once and keyed by image digest.** What a build is made of
and how it was built — the Go version, the main module, the dependency module
paths, and the allow-listed build settings — never changes, so the payload is
immutable given its key and the agent sends it once per distinct build. The
backend MUST upsert it by image digest and MUST NOT expect it on every flush or
treat a redelivery (after an agent restart) as a change. It carries no
`captured_at` for the same reason. Join it to workloads through
`go_inventory.records[].image_digest`. Each entry of `modules` is an object with
`path` and the `version` the toolchain recorded; one flagged `replaced` was
redirected by a `replace` directive, so its version names what the build
*required* rather than what is linked, and MUST NOT be presented as the running
version ([ADR 0048](adr/0048-findings-name-the-fields-they-need.md)).

**`go_build.settings` is a bounded allow-list, and its keys are optional**
([ADR 0019](adr/0019-build-settings-by-allow-list.md)). The agent keeps only the
settings enumerated in [`security.md`](security.md) §8 and discards the rest on
the node, so the backend MUST NOT expect any particular key to be present and
MUST NOT infer anything from an absent one. In particular `vcs.revision`,
`vcs.time` and `vcs.modified` are recorded by the Go toolchain only when the
build had a VCS working tree, which containerized builds frequently do not: their
absence is the common case and MUST NOT be surfaced as missing data or as a
coverage gap. The whole `settings` object is omitted when nothing was kept.

**`node_metadata.nodes[].architecture` is what `go_build.settings.GOARCH` is
compared against.** A build's target architecture and microarchitecture level
mean nothing on their own; the comparison is the finding. The field is the
kubelet's own value (`amd64`, `arm64`), and is omitted if the kubelet does not
report one.

### Delivery semantics

- **At-least-once.** The agent retransmits anything not acknowledged. The
  backend MUST deduplicate by natural keys (cluster, workload, container,
  window start, window length; profile identity) so that resends are
  harmless. Ingest MUST be idempotent.
- **Open-window snapshots supersede.** While a rollup window is open, the
  agent periodically ships a snapshot of its accumulating record so the
  data is fresh during the window, not an hour later. The backend MUST
  upsert snapshots by the natural key above: a newer snapshot of a window
  replaces an older one, and the final closed-window record replaces every
  snapshot. **Last write wins, and no payload carries an ordering field**
  ([ADR 0027](adr/0027-no-payload-ordering-field.md)): the agent's spool holds
  exactly one version of a natural key at a time, atomically replaced, so it
  never has two versions to offer. In exchange the agent MUST keep at most one
  request per natural key in flight — a resend of an unacknowledged payload
  re-reads the current version and is therefore the same or newer, never older.
  Snapshots have the same aggregate shape as any rollup; they do not relax §9
  (no raw time series).
- **Acknowledgment is a durability promise.** The backend MUST NOT
  acknowledge data it can still lose. The agent trims its local buffers only
  after acknowledgment.
- **Late and out-of-order data is normal.** The agent buffers through
  outages — in memory and in its spool, which is ephemeral (ADR 0026) — and
  uploads backlog on reconnection. The backend MUST accept
  historical timestamps and batch catch-up uploads, and SHOULD tolerate
  moderate agent clock skew — data is keyed by agent-side timestamps.
- **Gaps are normal too.** The agent's spool is ephemeral by decision, not by
  configuration (ADR 0007, ADR 0026): an agent restarted during an outage may
  reconnect with nothing older than its fresh data. Routine loss is bounded
  by the snapshot cadence. The backend MUST NOT treat a gap as an error and
  MUST NOT expect agents to re-supply history they no longer hold.
- **Declared limits.** Payload size and rate limits MUST be explicit in the
  protocol so the agent can chunk deterministically, not discover limits by
  failing.

## 5. Error semantics

The agent's failure behavior branches on the class of error, so the backend
MUST make the classes distinguishable:

| Class | Examples | Agent reaction the backend must support |
|---|---|---|
| Transient | unavailability, rate limiting, timeouts | Back off, buffer locally, retry. Backend MUST tolerate the resulting catch-up bursts |
| Identity | expired / revoked certificate, unknown cluster | Stop sending, attempt re-enroll if a valid token is present, otherwise degrade to local-only. MUST be clearly distinct from transient errors — misclassification causes retry storms or false local-only degradation |
| Permanent | malformed payload, unsupported schema version | Drop-and-log or hold-and-alert; never blind retry |

## 6. Compatibility

Enterprise agents upgrade slowly and stay installed for a long time.

- Ingest payloads are versioned. The backend MUST accept payloads from prior
  agent versions across a published support window (target: at least N-2
  minor versions) and MUST NOT require lockstep upgrades.
- Protocol changes are backward-compatible by default; a breaking change
  requires a deprecation period during which both forms are accepted.
- The backend SHOULD record each agent's reported version, making "which
  installs are behind" answerable centrally.

## 7. Control plane (operator-facing, agent-adjacent)

Not used by the agent itself, but required for its lifecycle at fleet scale
(see the Terraform/GitOps onboarding decision):

- Org-level API keys: long-lived, scoped, revocable independently of running
  agents; they authorize automation, never enter clusters.
- Cluster registry: create/read/update/delete with names and labels;
  idempotent creation by name (safe under `terraform apply` re-runs); token
  issuance MUST be a separate explicit action, not a side effect of reads.
- Operations: issue/rotate enrollment token, revoke certificate, re-enroll —
  all per cluster, all automatable.
- The control plane speaks REST/JSON with an OpenAPI definition — its
  consumers are humans and automation (Terraform provider, CLI, UI, later
  MCP), not the agent.
- **Fleet visibility.** The backend MUST expose, per cluster: expected vs.
  reporting, last-seen time, ingest lag, agent version, and fingerprint
  conflicts. "Created 50, reporting 47 — and which three" must be answerable
  without touching any cluster.

## 8. Isolation and retention

- Data MUST be partitioned by organization and cluster; nothing crosses org
  boundaries in any view, export, or API.
- The backend retains full history (it is the source of truth); retention
  policy is a product decision but MUST NOT rely on agents re-supplying old
  data — agents can't.
- Deleting a cluster record MUST have defined, documented consequences for
  its stored data (and MUST NOT be triggerable by the agent itself).

## 9. Explicit non-goals of this contract

The backend MUST NOT expect or attempt:

- pushing configuration, filters, or collection targets to agents — all
  agent behavior is defined in-cluster via Helm values
- addressing or connecting to anything inside a customer cluster
- receiving raw time series, raw stack samples, unfiltered profiles, secrets,
  or pod `env`/`args`/`command` — the agent never sends them, and their
  arrival indicates an agent bug, which the backend SHOULD flag rather than
  store
- coupling ingest availability to analysis availability: analysis being down
  MUST NOT cause data loss for connected agents
