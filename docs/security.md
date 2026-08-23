# Security Overview

This document describes what `runtime-agent` can access, what it collects, what
leaves the cluster, and which guarantees are enforced by Kubernetes itself
versus by the agent's own configuration. It is written for security review and
aims to be complete rather than flattering: known limitations are listed in
[§10](#10-known-limitations-and-honest-disclosures).

Two conventions, because the agent is not finished and this document describes
the designed system:

- **[planned]** marks a claim that is not built. It tells a reviewer the
  sentence is intent, not a property. **TBD** marks something still being
  verified before GA.
- This document states *what* is true; *why* is recorded once in the decision
  records under [`adr/`](adr/README.md) and linked from the claim. A fact kept
  in two places eventually disagrees with itself, which is what happened here
  (ADR 0024).

---

## 1. Design principles

1. **Read-only, with one named exception.** The agent holds no write verb on any
   Kubernetes resource except `get`/`update` on its own pre-created identity
   Secret, scoped by `resourceNames` to that single object — the product's only
   write grant, disclosed in the RBAC table of [§4](#4-kubernetes-api-access-rbac)
   and decided in [ADR 0008](adr/0008-identity-in-secret.md). It cannot modify
   workloads, nodes, or configuration. **[planned]** — no manifest emits that
   Role today.
2. **One-way data flow.** The agent pushes data out; there is no command or
   control channel from the backend to the agent. The backend cannot instruct
   the agent to do anything. All behavior is defined by the in-cluster
   configuration (Helm values → ConfigMap), which you own, version, and audit.
3. **Every access maps to a feature.** Disabling a feature removes the
   corresponding access. Nothing is requested "just in case."
4. **Filter as early as possible.** "Not collected" is better than "collected
   and discarded," which is better than "collected and not sent." For each
   filter, this document states the stage at which it applies.
5. **Single egress point.** Only the controller communicates outside the
   cluster, to one fixed domain, over mTLS **[planned]**. Node-level components
   never talk to the internet — that part holds today: the node role opens no
   socket except the one to the controller.

---

## 2. Architecture and trust boundaries

One binary, two roles:

```
runtime-agent controller   # 1 replica — talks to k8s API and pod pprof
                           #   endpoints; sole egress point
runtime-agent node         # DaemonSet (optional) — eBPF profiling and Go binary
                           #   detection on the node; talks only to the controller
```

- The controller is single-replica by design: it is the one writer of the
  identity Secret and the one holder of the cluster's backend identity. It runs
  as a Deployment today; ADR 0008 assumes a StatefulSet, and which of the two
  the chart ships is reconciled when the identity work lands (ADR 0022 §5).
- The node role sends data to the controller over the cluster network only.
- The controller exposes an in-cluster-only HTTP receiver for those node
  reports (ADR 0010): it accepts data pushes from the node DaemonSet and
  answers ack/error only. It is not reachable from outside the cluster and
  returns nothing the node acts on — the one-way rule ([§1](#1-design-principles),
  principle 2) applied one level down.
- The controller aggregates, filters, and ships data to
  `<backend endpoint, fixed domain>` over mTLS **[planned]**. Today it
  aggregates and filters, and writes the result to its local spool.
- Configuration is read exclusively from the Helm-rendered ConfigMap. The
  effective configuration is therefore always inspectable inside your cluster
  and diffable in your GitOps history.

---

## 3. Installation profiles

Three profiles with increasing levels of access. Pick the lowest that serves
your goals; profiles can be upgraded later.

| Profile | Components | Node privileges | What you get |
|---|---|---|---|
| `metrics-only` | controller | none | Cost and efficiency findings from usage metrics (kubelet counters via the API server, ADR 0006) and workload metadata |
| `pprof` **[planned]** | controller | none | Above + CPU/heap profiles pulled from services that already expose `/debug/pprof`. No pprof puller exists yet; nothing in the agent opens a `/debug/pprof` endpoint today |
| `ebpf` | controller + node DaemonSet | see [§7](#7-node-privileges) | Above + CPU profiles for services without pprof endpoints |

The node DaemonSet has two functions with very different privilege needs, kept
deliberately separate (ADR 0009): a **Go-binary scanner** (reads build info
from on-node executables — no eBPF, no Kubernetes API access) and the **eBPF
CPU profiler**. [§7](#7-node-privileges) documents each. The scanner is the
lower-privilege of the two and is what the current node role runs.

---

## 4. Kubernetes API access (RBAC)

All rules are `get`, `list`, `watch` — with **one write exception,
disclosed in the table below**: `get`/`update` on the agent's own
pre-created identity Secret, scoped by `resourceNames` in a namespaced Role
(ADR 0008) — **[planned]**, and the only row of this table that is. With
`persistence.enabled: true` that grant is not emitted at
all, and the chart is entirely write-free. Nothing else anywhere carries
`create`, `update`, `patch`, `delete`, or `deletecollection`.

The table below is the **controller's** access. The node role (the DaemonSet,
[§7](#7-node-privileges)) holds **no** RBAC at all — its ServiceAccount is
bound to nothing and its token is not mounted — so it appears in no row here.

| Resource | Verbs | Why |
|---|---|---|
| `pods` | get/list/watch | Map containers to workloads; read `requests`/`limits` from spec; detect OOM kills and count restarts from `status.containerStatuses[]` (`restartCount`, and the reason and exit code of `lastState.terminated`); read the `PodScheduled` and `DisruptionTarget` conditions to report why a replica is not running and when the cluster took one away; read `containerPorts` to locate pprof endpoints |
| `replicasets`, `deployments`, `statefulsets`, `daemonsets`, `jobs`, `cronjobs` | get/list/watch | Resolve the `ownerReferences` chain (Pod → ReplicaSet → Deployment) so findings are aggregated per workload, not per pod |
| `nodes` | get/list/watch | `allocatable`/`capacity` for node idle computation; labels `node.kubernetes.io/instance-type` and capacity-type (spot/on-demand) for the cost model; `status.nodeInfo.kernelVersion` to report whether nodes meet the kernel floor for the eBPF profile (`CAP_BPF` requires kernel 5.8+, see [§7](#7-node-privileges)) |
| `namespaces` | get/list/watch | Evaluate namespace allow/deny filters and the opt-out annotation |
| `nodes/proxy` | get | Poll each kubelet's `/stats/summary` and `/metrics/cadvisor` through the API server for usage counters: CPU, memory working set, CFS throttling, PSI where exposed (ADR 0006). **Honest disclosure:** this verb technically permits any kubelet GET endpoint through the API server, including node logs. The agent calls exactly the two stats paths above — auditable, since all kubelet access lives in a single poll loop |
| its own identity Secret **[planned]** | get, update (namespaced Role, `resourceNames`) | Persist the in-cluster-generated key and certificate across rescheduling (ADR 0008). Helm pre-creates the Secret; the agent owns only its content. No `create` (cannot be name-scoped), no `list`/`watch`/`delete` — the agent can neither enumerate nor touch any other Secret. Not emitted when `persistence.enabled: true` |

**Authenticating node reports adds no RBAC.** When the node DaemonSet is
installed (ADR 0010), the controller authenticates each node's projected
ServiceAccount token by verifying its signature against the cluster's published
JWKS **locally**. It does **not** call `TokenReview`, so no `create` on
`authentication.k8s.io/tokenreviews` — or any other verb — is added for this.
Resolving the cluster's OIDC discovery and JWKS endpoints is a read-only
non-resource URL `GET`, granted cluster-wide to ServiceAccounts by the built-in
`system:service-account-issuer-discovery` role on typical clusters; it is a
read, never a write.

### Cluster-wide vs. namespace-scoped installation

- **Cluster-wide (default):** a single ClusterRole with the rules above.
- **Namespace-scoped:** Role + RoleBinding only in namespaces you list;
  no ClusterRole is created. This is a hard boundary enforced by the API
  server, not by agent configuration.

  Honest trade-off: `nodes` and `namespaces` are cluster-scoped resources, so
  in this mode the agent cannot compute node idle capacity, cannot read
  instance types for pricing, and cannot reconcile totals against your cloud
  invoice. It reports requests-vs-usage findings for the permitted namespaces
  only.

---

## 5. Network access

| Access | Direction | Why | Notes |
|---|---|---|---|
| Pod pprof endpoints **[planned]** | controller → pods | Pull `/debug/pprof/profile` and `/debug/pprof/heap` from Go services that already expose them | Only pods that pass the workload filters are probed; ports taken from `containerPorts`, never blind scans. `pprof`/`ebpf` profiles only. Not built: the agent opens no connection to a pod today |
| kubelet stats, proxied | controller → API server | Poll `/stats/summary` and `/metrics/cadvisor` on every kubelet for usage counters (ADR 0006) | Goes through the API server proxy (`nodes/proxy`, §4) — the agent opens **no direct connection to kubelets**. A direct-kubelet transport for very large clusters would be a documented change here, not a silent one |
| Backend egress **[planned]** | controller → one fixed domain | Ship aggregated rollups and filtered profiles | mTLS, pinned domain. The only cross-boundary connection in the system, and it does not exist yet: the controller writes payloads to its local spool and ships nothing. A NetworkPolicy restricting controller egress to this domain plus in-cluster targets ships with the chart **[planned]** — no chart exists yet either (`deploy/` holds raw manifests) |
| Node → controller reports | node → controller, in-cluster only | Deliver on-node Go build-info findings and captured profiles for aggregation (ADR 0010, ADR 0011), and ask which pods on this node passed your filters (ADR 0015) | Plain HTTP on the cluster network; the node always initiates and authenticates with a projected controller-audience ServiceAccount token, validated locally (no `TokenReview`). The report endpoints answer ack/error only. The **scope** query answers from the controller's already-filtered pod index, so it can only narrow what the node scans — it cannot name a pod your filters exclude. The **profiling-target** query is the one reply the node acts on; see [§7.2](#72-the-ebpf-cpu-profiler-opt-in-ebpf-profile-adr-0011) for what bounds it. The receiver is not reachable from outside the cluster; a NetworkPolicy restricting it to the DaemonSet ships with the chart **[planned]**. The DaemonSet has **no** external egress. Honest disclosure: the token and the (already-filtered) facts travel in cleartext in-cluster; a party that can already sniff pod traffic could replay the short-lived token to post fabricated inventory facts for this one cluster — bounded, one-way, no read or control capability (ADR 0010) |

---

## 6. Agent identity and credential lifecycle **[planned]**

**None of this section is built.** The agent holds no backend credential today
and opens no connection outside the cluster; it collects into its local spool
and stops there. What follows is the designed model, decided in
[ADR 0005](adr/0005-two-tier-identity.md) and
[ADR 0008](adr/0008-identity-in-secret.md), which carry the reasoning.

- **Two tiers, so no long-lived secret is shared across clusters.** An **org API
  key** lives in your automation and never enters a cluster; it issues
  short-TTL, per-cluster **enrollment tokens**, and a token is exchanged once
  for a **client certificate**. A leaked token exposes one cluster for its TTL;
  a leaked org key is revoked in one action without touching a running agent,
  because agents run on certificates.
- **The private key is generated in-cluster and never leaves it.** On first
  start the controller generates a key pair and submits a CSR authenticated by
  the enrollment token, including a cluster fingerprint (the UID of
  `kube-system`) so a token applied to the wrong cluster is detected rather than
  silently mixing data. Enrollment is an outbound request the agent initiates —
  it creates no control channel (principle 2 in [§1](#1-design-principles)).
- **The key lives in a pre-created, name-scoped Secret** — the product's one
  write grant ([§4](#4-kubernetes-api-access-rbac)) — so identity survives
  rescheduling without a volume. Honest consequence: the key is then in etcd and
  in any backup containing Secrets. Whoever reads it can impersonate this
  cluster's telemetry stream — data poisoning of one cluster, nothing more,
  since the protocol is one-way and carries no read or control capability —
  recovered by revoke plus re-enroll, with data history continuous.
- **Certificates renew over the existing mTLS session**; the backend can revoke
  any cluster's at any time. With no valid certificate and no valid token the
  agent degrades to local-only: it keeps collecting and spooling, ships nothing,
  logs the condition, and neither crash-loops nor retry-storms.
- **The backend's private CA is pinned**, not the system trust store, so a
  TLS-intercepting proxy cannot impersonate the backend. Such a proxy also
  cannot pass the mTLS session through: with egress interception, the backend
  domain needs a bypass rule. This is an install prerequisite, not a detail.

### Storage

The controller keeps a small local volume — an `emptyDir` by default;
`persistence.enabled: true` swaps in a PersistentVolume (~2Gi) for installations
that want unacknowledged data to survive rescheduling
([ADR 0007](adr/0007-optional-durability.md)). The volume holds exactly one
thing:

- **The spool of unshipped payload batches** — rollups, metadata and
  allow-listed profiles, held until delivery is acknowledged and deleted
  immediately after, with a maximum-age cap so an extended outage cannot fill
  the volume. Only data that has already passed the filters — data approved to
  leave the cluster — is ever written there.

Everything else the collector keeps — counter baselines, open windows,
profiling rotation state — is held **in memory only**. ADR 0003 placed a
checkpoint file next to the spool; [ADR 0022](adr/0022-registry-in-code-and-declared-amendments.md)
records that it was never built and will not be. Nothing about your cluster is
accounted for on disk outside the spool.

- The volume is for continuity, not truth: everything on it is reconstructible,
  so it needs no backups.
- **Configuration is never cached on it.** Filters are read from the ConfigMap
  at every start; if the configuration is unavailable the agent does not
  collect. A stale filter set cannot resurrect from disk.
- Encryption at rest is your storage layer's: point the PVC at an encrypted
  StorageClass.
- With the default `emptyDir`, a rescheduled pod loses only the unacknowledged
  spool — bounded by the flush cadence in normal operation, and by the outage
  span if the backend was unreachable (ADR 0007).

---

## 7. Node privileges

The DaemonSet is not installed at all in `metrics-only` and `pprof` profiles.
It has two functions with different privilege needs, documented separately
below (ADR 0009). Whichever runs, one property holds for both: **the node role
has no Kubernetes API access whatsoever.** Its ServiceAccount is bound to no
Role, RoleBinding, ClusterRole, or ClusterRoleBinding — not one RBAC rule
exists for it — and `automountServiceAccountToken: false` keeps the **default
API token** out of the container. When the node delivers findings to the
controller (ADR 0010) it mounts exactly one credential: a projected token
**audience-bound to the controller**, which the API server rejects (wrong
audience) and which grants nothing anyway (the ServiceAccount holds zero RBAC).
So the node still cannot reach the API — now by two independent barriers, wrong
audience *and* no grant — and it appears in none of the RBAC tables in
[§4](#4-kubernetes-api-access-rbac) because it holds nothing there. This is
enforced by the API server (an unbound identity cannot authorize anything), not
by agent configuration.

### 7.1 The Go-binary scanner (the current node role)

This is what the node DaemonSet runs today. It enumerates processes under
`/proc`, reads the Go build information embedded in each executable, ties it to
a pod and container through the process cgroup, filters infrastructure on the
node, and reports the result. It loads no eBPF; the only socket it opens is the
outbound connection to the controller to deliver its findings (ADR 0010) — never
to the internet, never to the API server.

| Privilege | Why |
|---|---|
| `hostPID: true` | See node processes and open their `/proc/<pid>/exe` to read Go `buildinfo` (Go version, module path, dependencies, whether `-pgo` was applied) without entering containers |
| `CAP_SYS_PTRACE` | The **only** added capability. Container processes are non-dumpable, so the kernel's ptrace access check blocks reading another container's `/proc/<pid>/exe` even for a same-UID root reader — verified empirically. This resolves the "TBD" this document previously carried here: the capability is required, and it is **not** an eBPF capability |
| host `/proc`, read-only | Mounted at `/host/proc` so the scanner reads the node's process table. No other host path is mounted |

What it reads, and only that: `/proc/<pid>/exe` (build info) and
`/proc/<pid>/cgroup` (the pod UID and container ID the kubelet encodes into the
cgroup path). See [§8](#8-data-collected-and-data-leaving-the-cluster) for the
data and the on-node filter.

Compensating controls, all set in the shipped manifest
(`deploy/node-daemonset.yaml`):

- `privileged: false`, `allowPrivilegeEscalation: false`.
- Read-only root filesystem.
- `seccompProfile: RuntimeDefault`.
- **All capabilities dropped except `SYS_PTRACE`.** No `CAP_BPF`, no
  `CAP_PERFMON`.
- Runs as UID 0 (needed to match the credentials of the root processes it
  reads); bounded by all of the above and by the absence of API access.

### 7.2 The eBPF CPU profiler (opt-in `ebpf` profile, ADR 0011)

Off by default; enabled only in the `ebpf` profile. When enabled it adds, on top
of the scanner's privileges, exactly two capabilities and one read-only host
mount — never `privileged`:

| Privilege | Why |
|---|---|
| `CAP_BPF` + `CAP_PERFMON` | Load eBPF programs and perform perf sampling for CPU profiles. These are the *minimum* for the embedded profiler; `privileged` is **not** used and `allowPrivilegeEscalation` stays `false`, so the container cannot acquire capabilities beyond these two |
| host `/sys/kernel/btf`, read-only | Kernel BTF, required by the profiler's CO-RE eBPF programs for stack unwinding and on-node symbolization. This is the one additional host path beyond the scanner's `/proc`; no writable host mount is added |

**Kernel floor with a graceful refusal.** `CAP_BPF`/`CAP_PERFMON` exist only on
kernel 5.8+, and the profiler needs BTF. On a kernel that lacks either, the
`ebpf` profile does not start the profiler, reports the unmet requirement, and
increments a counter — the Go-binary scanner keeps running. The profile is
degraded and visible, never silently escalated to `privileged`.

**Symbolization happens on the node.** Your binaries are never copied or
uploaded. Stack addresses are resolved to function names locally against the
running binary, and only symbolized, allow-list-filtered profiles (see
[§8](#8-data-collected-and-data-leaving-the-cluster)) leave the node — and only
towards the in-cluster controller.

**What decides who gets profiled.** Profiling is off unless you enable it and
name eligible namespaces; the eligible set is empty by default, so nothing is
profiled until you opt a namespace in. On its own interval the node asks the
controller which of the eligible workloads rank highest by consumption, and the
reply is a list of container IDs on that node — the one node↔controller reply
the node acts on (ADR 0011 §3).

Two answers must admit a container before its samples are shipped: the targets
reply, and the scan scope of [§10.2](#102-node-level-visibility-cannot-be-namespace-scoped) —
the same set that gates the binary scanner. A pod whose executable the scanner
may not open is not profiled either, and a node that cannot reach the controller
profiles nothing rather than everything it was last told.

**Which bound is enforced where, and what each one is worth** — stated as a
table because the distinction is the whole security argument, and it was
previously stated wrongly (ADR 0025):

| Bound | Enforced from | Stops a buggy controller | Stops a hostile one |
|---|---|---|---|
| Symbol allow-list — which module prefixes may leave in a frame | the node's own ConfigMap | yes | **yes** |
| Overhead ceiling, capture duration, interval | the node's own ConfigMap | yes | **yes** |
| Max targets per window | the node's own ConfigMap | yes | **yes** |
| Profile validation (parses, `cpu`/`nanoseconds`, a service function present) | the node's code | yes | **yes** |
| The container exists on this node | the node's `/proc` | yes | **yes** |
| Eligible namespaces and workloads | the controller's ConfigMap | yes | no |
| Scan scope | the controller's reply | yes | no |

The bottom two are facts the node receives rather than facts it holds, so they
bound a controller that is wrong and not one that is lying — a hostile
controller would simply send a different answer. **This is not a gap that can be
closed** while the node holds zero Kubernetes API access (ADR 0009): every
namespace fact reaches it through the controller, so no node-side re-check of a
controller-supplied label can constrain the controller. Giving the node the
eligible set to check would look like a safeguard and be none.

What that leaves is the top five rows, and they are genuinely unforgeable. The
symbol allow-list is the load-bearing one: it is the reason a stack trace is
defensible to ship at all, and it is enforced on the node from a file your Helm
release owns.

---

## 8. Data collected and data leaving the cluster

This is usually the most important section for review. Data falls into three
classes with different sensitivity and different default policies:

| Data class | Examples | Sensitivity | Default policy |
|---|---|---|---|
| Resource metrics | CPU usage, working set, throttling ratios, requests/limits, node allocatable | Low (numbers) | Collected for everything visible, minus the infrastructure deny-list and your filters |
| Workload metadata | Workload names, namespaces, image digests, Go version, module path, node zone and instance type | Medium | Same as metrics |
| Object history (journal) | Pod names, container names, restart counts, termination reasons and exit codes | Medium | Same as metrics. This is the one class that names individual **pods** — see below |
| Profiles (stack traces) | Function names, call graphs | **High** — function names reveal the structure of your code | **Explicit allow-list only.** A stack trace never leaves the cluster unless its module path is on the allow-list you configured |

### Every payload the agent produces

This is the complete list — there is no other channel and no other shape. It is
generated from the same registry the code writes payloads through
(`internal/sink/registry.go`), and a test fails if a kind ships without
appearing here (ADR 0022).

| Payload | What it carries | Names pods? |
|---|---|---|
| `usage_snapshot` | The still-open hour's resource rollup: per (namespace, workload, container) histograms of CPU and memory, throttling and PSI counters, plus how much of the window was observed. Shipped once a minute, each replacing the last | no |
| `usage_window` | The same shape, final, when the hour closes | no |
| `oom_kill` | One out-of-memory kill: namespace, pod, container, workload, when it finished, exit code, restart count, and the memory limit in force | **yes** |
| `container_restarts` | Per (namespace, pod, container) and hour: restart count, breakdown by termination reason, how many restarts had no reason visible, last exit code | **yes** |
| `pod_disruptions` | Per hour: the pods the *cluster* removed — preempted, evicted under node pressure, drained, or removed via the eviction API — with the node and the instant | **yes** |
| `workload_metadata` | Declared shape per (namespace, workload, container, image digest): image, requests, limits, ports, QoS, replica counts by phase, by node, and by unscheduled reason | no |
| `node_metadata` | Per node: name, size, instance and capacity type, zone, region, kernel version, CPU architecture | no |
| `go_inventory` | Per (namespace, workload, container): Go version, main module, image digest, PGO flag, plus a fleet-coverage block | no |
| `go_build` | Per image digest, written once: Go version, main module, dependency module **paths**, allow-listed build settings | no |
| `ebpf_profile` | One capture: allow-list-filtered symbolized pprof bytes, keyed by workload, image digest and capture window | no |

Each declares its provenance in a `source` field — `structural` (read from a
spec), `measured` (polled from the kubelet), `journal` (from object history), or
`sampled` (a profiler's estimate) — so the backend can never merge
epistemically different data under one key
([ADR 0012](adr/0012-payload-registry-and-provenance.md)). The rest of this
section describes each in detail.

### Field minimization

- The agent **never reads Secrets or ConfigMaps** (no RBAC for them exists).
- From pod specs, the agent keeps only `metadata`, `spec.containers[].resources`,
  `spec.containers[].ports`, `ownerReferences`, and `status`. It explicitly
  discards `env`, `args`, and `command` before anything is stored or
  transported, because these fields frequently contain inline credentials.
  From `status` it reads the container image digest
  (`status.containerStatuses[].imageID`, kept as the content digest only), the
  restart counter (`restartCount`), the *reason* and *exit code* of a
  terminated container state, and the *reason* and *transition time* of exactly
  two conditions — `PodScheduled` and `DisruptionTarget` — plus `status.reason`
  when it says `Evicted`. Nothing else. **Messages are never read**, neither a
  termination's nor a condition's: unlike a reason, which Kubernetes picks from
  its own vocabulary, a message is free text and routinely names nodes, taints,
  and other teams' workloads. A scheduler message such as "0/12 nodes are
  available: 3 node(s) had untolerated taint {dedicated: payments-gpu}" never
  leaves the cluster; the reason `Unschedulable` does.
- From node objects, the agent keeps only size (`allocatable`/`capacity`), the
  instance-type and capacity-type labels, the topology labels
  (`topology.kubernetes.io/zone` and `/region`, with their deprecated
  `failure-domain.beta.kubernetes.io/` equivalents), and two fields of
  `status.nodeInfo`: `kernelVersion` (a version string, used to report
  eBPF-profile readiness) and `architecture` (`amd64`/`arm64`, what a build's
  target architecture is compared against, ADR 0019). No other node labels,
  annotations, addresses, or `status` fields are read. Zone and region are how resource usage is
  attributed to a failure domain and to the corresponding lines of your cloud
  bill; they are labels your cluster already publishes.

### Workload and node metadata (ADR 0012)

Both are snapshots of current state that replace their predecessor rather than
accumulating history, and neither names a pod: replicas are counted, not listed.
A pod with several containers appears as one `workload_metadata` record per
container, each repeating the same `pod` block — the block is pod-scoped and is
nested so that summing it across a pod's containers is visibly meaningless
([ADR 0014](adr/0014-scoped-facts-are-nested.md)).

That `pod` block also counts the replicas that are *not* on a node, by the
reason the scheduler gave (`Unschedulable`, `SchedulingGated`, `SchedulerError`,
or `other`). The shortfall was always visible as the gap between `replicas` and
the `nodes` breakdown; this says why, still without naming a pod
([ADR 0021](adr/0021-pod-lifecycle-journal.md)).

Both are limited to what passes your filters — a pod excluded by a namespace
filter or an opt-out annotation is absent from the snapshot entirely, and its
identity never appears in either payload.

Both also carry `captured_at`, the instant the snapshot was assembled, as does
the Go inventory below. It timestamps the agent's own work, not anything in your
cluster, so that a stale snapshot can be recognized rather than assumed current
([ADR 0017](adr/0017-build-facts-keyed-by-digest.md)).

### Object history, and the one place pods are named (ADR 0020)

`oom_kill`, `container_restarts` and `pod_disruptions` report what the cluster's
own object status records about a container's history — see the table above for
what each carries. All three are `journal` provenance, and all three name the
**pod**. The line between the last two is that `pod_disruptions` is what the
*cluster* did to a pod (preempted, evicted under pressure, drained, removed via
the eviction API); a pod that failed on its own, its container exiting non-zero,
belongs to the restart journal instead.

**Why the pod is named here and nowhere else.** Every other payload counts
replicas instead of listing them, because a workload-level number answers the
question. A crash loop is different: "one of your twelve replicas restarts every
thirty seconds" is not actionable without knowing which one. The pod name is the
smallest identifier that makes the record useful, and it is a name your cluster
generated, never workload content ([ADR 0020](adr/0020-container-restart-journal.md) §2).

If you would rather it did not leave, the controls are the ones in
[§11](#11-your-controls-and-what-the-agent-says-about-itself): a namespace
filter or the `rebuildstack.co/collect: "false"` annotation. An excluded pod
produces no journal record at all — the exclusion applies before any of this is
formed, and also removes what was already collected.

The reason breakdown uses a fixed set of names (`OOMKilled`, `Error`,
`Completed`, `ContainerCannotRun`, `ContainerStatusUnknown`, `DeadlineExceeded`,
`StartError`, `Evicted`); anything else your container runtime reports is
counted under `other` rather than passed through, so the payload's shape stays
bounded. Disruption reasons are bounded the same way. Termination and condition
*messages* are never read (see field minimization above).

### What the agent says about its own collection (ADR 0013)

Usage payloads carry, alongside the numbers, what the agent actually observed
while producing them. This is metadata about the agent's own polling — it
describes no workload and identifies nothing in your cluster:

- an `observation` block per payload: the poll cadence, cumulative counts of
  kubelet requests attempted and failed, and which kubelet signals this cluster
  exposes (e.g. whether PSI is available);
- per record: how much of the window that container was observed for, and how
  many observations carried each signal.

The purpose is honesty about gaps: without it, a container that was never
throttled and a container whose node could not be scraped produce identical
records, and the distinction is unrecoverable once the moment has passed
([ADR 0013](adr/0013-observation-completeness.md)). No pod, node, or workload is
named in this data; the failure counts are cluster-wide totals.

### Profile filtering (applies on the node, before egress — ADR 0011)

- Stack frames are filtered by Go module path on the node that captured them.
  The policy has three parts: frames whose module path is on **your client
  allow-list** are kept; **standard-library and `runtime` frames are always
  kept** (they carry no structure of your code and make a profile readable);
  **third-party dependency frames are governed by config and dropped by
  default.** Everything else (Kubernetes components, service meshes,
  observability stacks, this agent itself) is dropped. Only the kept frames and
  an aggregate count of what was dropped leave the node — never the identities
  of dropped functions.
- eBPF profiles are **validated on the node before they are queued to leave**: a
  profile is shipped only if it parses, carries a `cpu`/`nanoseconds` sample
  type, and has a service (non-`runtime.*`) function in its top. A profile that
  fails is dropped and counted, never sent. What does leave is keyed as a
  profile payload — pprof bytes plus (namespace, workload, container, image
  digest, time window). It declares `source: sampled`: the provenance field
  says what kind of claim the numbers are — an estimate from a fraction of the
  window's instants — while which capture produced them is what the payload
  kind names, `ebpf_profile` (ADR 0023).
- These are **flame-graph / hot-function profiles, not PGO profiles.** The eBPF
  profiler's output omits data Go's PGO requires (`Function.start_line`, inline
  frames), so it is used for attribution and coverage only; PGO, if ever
  offered, comes from native pprof, not from this path (ADR 0011).
- Aggregated metric rollups are hourly mergeable histograms per workload —
  no raw per-second samples are shipped.
- Raw, unfiltered samples exist only in a 24–48h ring buffer on an ephemeral
  `emptyDir` volume, for debugging and flame graphs. They are never
  transported and never written to the persistent volume — unfiltered stack
  traces do not survive pod deletion. Profiles that passed the allow-list
  (i.e. are approved to leave the cluster) may sit in the spool on the
  persistent volume until their delivery is acknowledged, and are deleted
  right after.

### On-node binary scanning (node role, ADR 0009)

The node role reads Go build information from executables on the node and
applies the filter-early rule on the node, before any record is formed:

- **Scanned at all — only pods that passed your filters** (ADR 0015). Before
  anything else, a process's cgroup is resolved to its pod, and a pod outside the
  scope the controller supplied is skipped with its executable unopened. A node
  that has no scope — controller unreachable, or the endpoint not configured —
  scans nothing. See §10.2.
- **Kept — customer workload binaries.** For a Go process whose main module is
  *not* infrastructure, the scanner keeps the Go version, the main module path,
  the dependency module paths (paths only — dependency *versions* are discarded
  on the node and never leave it), an allow-listed subset of the build settings
  (below), and the pod UID and container ID parsed from the process cgroup. This
  is the same "medium sensitivity" metadata class as the rest of the workload
  metadata above — module and version strings, never source, never environment,
  never arguments.
- **Build settings — an allow-list, enumerated here in full** (ADR 0019). Build
  settings are the one place where strings written by whoever ran the build
  enter the agent, so the scanner keeps only these and discards everything else
  on the node:

  | Setting | What it is |
  |---|---|
  | `CGO_ENABLED` | `0` or `1` |
  | `GOARCH` | target architecture |
  | `GOAMD64`, `GOARM64`, `GOARM` | target microarchitecture level |
  | `-race` | built with the race detector |
  | `-trimpath` | build paths stripped from the binary |
  | `vcs` | which VCS stamped the build (`git`, …) |
  | `vcs.revision` | the commit the binary was built from |
  | `vcs.time` | that commit's timestamp (not the build time — Go does not record it) |
  | `vcs.modified` | whether the working tree was dirty |

  Everything outside this list is discarded on the node, including
  **`-ldflags`, `-gcflags`, `-asmflags` and `-tags`** — free-form flags that
  routinely carry build-machine paths, internal hostnames and injected version
  strings — and the **value of `-pgo`**, which is a path on the build machine.
  Whether PGO was applied survives as a boolean; where the profile lived does
  not. A value longer than 128 characters is dropped whole rather than
  truncated. `vcs.revision` is a commit identifier from your repository: it
  carries no source, is meaningless without access to that repository, and is
  frequently absent, which is normal rather than a gap. Why an allow-list and
  not a deny-list is [ADR 0019](adr/0019-build-settings-by-allow-list.md).
- **Dropped — infrastructure.** A main module on the built-in deny-list
  (`k8s.io/`, `sigs.k8s.io/`, the container runtime, the CNI, `go.etcd.io/`,
  `github.com/coredns/`, `github.com/prometheus/`, `github.com/grafana/`, … and
  this agent's own `github.com/RebuildStackCo/`) is dropped on the node. Its
  identity — module path, pod UID, container ID — is never recorded, only
  counted.
- **Counted, never identified.** Five aggregate counters describe everything
  not kept: processes scanned, Go binaries found, skipped as out of scope,
  filtered as infrastructure, and unreadable (a real executable with no
  recoverable Go build info — a non-Go program, or a Go binary whose build info
  was removed). No identity of a filtered or unreadable process leaves as
  anything but a number (invariant 6).

The controller joins the kept facts against its workload inventory (ADR 0010) —
pod UID → workload, container ID → container name and image digest — into
`go_inventory` and `go_build`, described in the table above. Four properties of
that join matter for review:

- **`go_build` carries module paths only, never versions.** The agent does not
  collect the version of any dependency, so this is not, and cannot be used as,
  a vulnerability-scanning feed ([ADR 0017](adr/0017-build-facts-keyed-by-digest.md) §2).
- **An inventory record exists only while its workload does.** On every flush
  the inventory drops whatever the filtered pod index no longer holds, so a
  deleted or opted-out workload leaves the payload rather than lingering as a
  last known state ([ADR 0018](adr/0018-inventory-records-live-only-while-their-workload-does.md)).
- **A fact that cannot be resolved is counted, not guessed.** Informer lag or a
  filtered pod makes a fact *unjoined*: it is dropped, and its pod UID,
  container ID and module path never appear — only the count. A fact whose
  container has no image digest yet is *undigested*: the inventory record is
  kept, the build payload is not, because there is no build to key it to.
- **The `coverage` block is cluster-wide counts and names nothing**: how many
  nodes have delivered a report, and how many facts were received, joined, and
  not.

### What the agent reports about your filters

The rule: full information about what is collected, only aggregate
information about what is not.

- **Names of excluded objects are never transmitted** — not namespaces, not
  workloads, not deny-listed module paths. The only filter content that
  leaves the cluster is the profile module-path allow-list, which is already
  self-evident from the profiles it admits.
- **Counts of what was excluded are.** The agent tallies pods observed and pods
  excluded by each filter, and the Go inventory's `coverage` block and the usage
  payloads' `observation` block carry the collection-side equivalents. Every one
  of them is a number; none names anything.
- Node-level totals (allocatable, aggregate node usage) are collected in
  cluster-wide mode regardless of workload filters. They are not
  attributable to individual workloads and are required to reconcile total
  cluster cost against your invoice.
- **[planned]** A payload carrying the per-filter exclusion counts, and a
  fingerprint of the effective filter configuration with the time it last
  changed, so any upload is attributable to the configuration active at that
  moment. The counters exist in the agent; no payload carries them out yet.

### Transport **[planned]**

Nothing is transmitted today: payloads are written to the local spool and
deleted only by its maximum-age sweep. The designed transport is
controller → backend over mTLS to a single fixed domain, carrying the payload
kinds enumerated above and nothing else.

---

## 9. What the agent does NOT request

Stated explicitly so it does not have to be asked:

- No `secrets`, no `configmaps` — reading either is impossible, no RBAC for
  them exists anywhere.
- No `pods/exec`, `pods/attach`, `pods/portforward`.
- **No write verb except one**, and it is named: `get`/`update` on the agent's
  own identity Secret, scoped by `resourceNames`
  ([§4](#4-kubernetes-api-access-rbac), and **[planned]** — not emitted today).
  Nothing else in the product carries `create`, `update`, `patch`, `delete` or
  `deletecollection`, so the agent cannot change your workloads, nodes or
  configuration.
- No cloud provider credentials, IAM roles, or billing API access. Node
  pricing uses a static price table plus your stated discount.
- No external egress from nodes — only the controller crosses the boundary.
- No access to your monitoring stack: not Prometheus, Thanos, Mimir, or any
  other metrics store, neither as an option nor to backfill history at install
  time ([§10.1](#101-the-agent-knows-nothing-about-the-time-before-it-was-installed)).
- No API access from the node role at all
  ([§7](#7-node-privileges)) — zero RBAC, default token not mounted.
- No dynamic configuration from the backend — the agent cannot be
  reconfigured remotely.

---

## 10. Known limitations and honest disclosures

### 10.1 The agent knows nothing about the time before it was installed

Every measurement the agent reports it made itself, from the kubelet counters
of ADR 0006. It does **not** read your Prometheus — not as an option, not for a
one-time backfill — so a cluster installed today has no usage data for
yesterday, and a finding that needs a long observation window has to wait for
that window.

This is a deliberate refusal, not an unbuilt feature, and it is a refusal about
*measurability*: a Prometheus series carries no record of the recording rules,
downsampling and relabelling that shaped it, so it could declare neither its
provenance nor its completeness while being joined, under the same keys, with
data that declares both. Its tenancy is also weaker than every other path here —
the endpoint typically serves all namespaces regardless of the caller's
permissions. The full argument is
[ADR 0016](adr/0016-no-prometheus-as-a-source.md).

What does not need history still works on day one: declared requests and limits,
QoS, topology and placement, and the object journal are all true at install
time. What genuinely needs observation reports how long it has been observing,
so its weight is visible rather than assumed.

### 10.2 Node-level visibility cannot be namespace-scoped

With `hostPID` and eBPF, the node component technically observes all processes
on a node, including pods from namespaces outside your filters — this is a
property of node-level profiling, not of this agent. The mitigation is the
collection-stage filter: samples from processes whose module path or namespace
is not allow-listed are discarded on the node, before transport to the
controller. If this is not acceptable, use the `pprof` or `metrics-only`
profile, where no node component exists.

**How the node knows your namespaces**
([ADR 0015](adr/0015-node-scan-scope.md)). The node holds no Kubernetes API
access, and a process's cgroup tells it a pod UID, never a namespace — so it
cannot evaluate your filters itself. On each pass it asks the controller which
pods on this node passed them, and scans only those. Two properties follow:

- **A process outside that set has its executable never opened.** Scope is
  checked on the cgroup first, so no module path is ever extracted for a pod you
  excluded — not collected, as opposed to collected and discarded. Only an
  aggregate count of skipped processes is reported.
- **It fails closed.** A node that cannot reach the controller, or is deployed
  without the scope endpoint, scans nothing rather than falling back to scanning
  everything. An outage costs one scan pass, never a widening of what is
  collected.

A process belonging to no pod — the kubelet, a systemd unit, anything outside
the pod hierarchy — is never in scope, because it is in no namespace and so no
namespace filter of yours can permit it.

### 10.3 pprof endpoint probing **[planned]**

Not built — the agent makes no request to a pod today. When it does: locating
pprof endpoints means the controller makes HTTP requests to pods, which network
monitoring may flag as internal scanning. Probing will be restricted to
workloads that pass your filters and to ports declared in `containerPorts`.
Coordinate with your security team before enabling the `pprof` profile; the
`metrics-only` profile performs no probing.

---

## 11. Your controls, and what the agent says about itself

### Your controls

Three, and they are the complete list:

| Control | Where | What it does |
|---|---|---|
| **Namespace allow / deny lists** | Helm values → ConfigMap (`filters.namespaces`) | Names, `*` wildcards permitted. An empty allow list admits every namespace; a non-empty one admits only matches. Deny applies on top and wins on conflict |
| **Namespace opt-out annotation** | on the Namespace object | `rebuildstack.co/collect: "false"` excludes every pod in it |
| **Pod opt-out annotation** | on the Pod object | the same annotation excludes that pod |

Profiling has its own, stricter gate on top of these: it is off unless enabled,
and its eligible namespaces are an allow-list that is **empty by default**
([§7.2](#72-the-ebpf-cpu-profiler-opt-in-ebpf-profile-adr-0011)).

There is **no label-selector filter.** Earlier revisions of this document listed
one; it was never decided in an ADR and never existed in the configuration, so
it is removed rather than left standing
([ADR 0024](adr/0024-security-doc-states-what-adrs-state-why.md) §3).

### What the agent says about itself

- **Coverage counts:** the agent tallies pods observed and pods excluded by each
  filter, so filtering behavior is verifiable from output rather than only from
  configuration. Delivering those counts out of the cluster is
  **[planned]** — see [§8](#8-data-collected-and-data-leaving-the-cluster).
- **Startup self-audit [planned]:** enumerate the agent's effective permissions
  via `SelfSubjectRulesReview` and log exactly what it can and cannot see. Not
  built. What does hold today: a 403 degrades gracefully with a log line, never
  a crash-loop or retry storm.

### Opt-out

Any namespace or pod can exclude itself without touching the agent's
configuration:

```yaml
metadata:
  annotations:
    rebuildstack.co/collect: "false"
```

The exclusion applies at the collection stage, and it applies to what was
already collected. The pod leaves the agent's index immediately, it stops being
named in the scan scope the controller gives nodes (§10.2), and the next
snapshot of each payload no longer carries it — including the Go inventory,
which is assembled from facts nodes push and therefore has to be told to forget
rather than simply stop learning (ADR 0018). Nothing about an opted-out pod
survives in a later payload; a workload that stops being collected disappears
from the next snapshot, it does not linger as a last known state.

---

## 12. Reporting a vulnerability

Contact: **TBD** (security contact will be published before the first external
deployment). Please do not open public issues for suspected vulnerabilities.
