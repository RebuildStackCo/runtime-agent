# Security Overview

This document describes what `runtime-agent` can access, what it collects, what
leaves the cluster, and which guarantees are enforced by Kubernetes itself
versus by the agent's own configuration. It is written for security review and
aims to be complete rather than flattering: known limitations are listed in
[§10](#10-known-limitations-and-honest-disclosures).

Status: living document, maintained alongside the code from day one. Items
marked **TBD** are being verified and will be resolved before GA.

---

## 1. Design principles

1. **Read-only.** The agent holds no write verbs on any Kubernetes resource and
   cannot modify workloads, nodes, or configuration. It observes and reports.
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
   cluster, to one fixed domain, over mTLS. Node-level components never talk
   to the internet.

---

## 2. Architecture and trust boundaries

One binary, two roles:

```
runtime-agent controller   # StatefulSet, 1 replica — talks to k8s API and
                           #   pod pprof endpoints; sole egress point
runtime-agent node         # DaemonSet (optional)   — eBPF profiling and Go binary
                           #   detection on the node; talks only to the controller
```

- The node role sends data to the controller over the cluster network only.
- The controller exposes an in-cluster-only HTTP receiver for those node
  reports (ADR 0010): it accepts data pushes from the node DaemonSet and
  answers ack/error only. It is not reachable from outside the cluster and
  returns nothing the node acts on — the one-way rule ([§1](#1-design-principles),
  principle 2) applied one level down.
- The controller aggregates, filters, and ships data to
  `<backend endpoint, fixed domain>` over mTLS.
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
| `pprof` | controller | none | Above + CPU/heap profiles pulled from services that already expose `/debug/pprof` |
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
(ADR 0008). With `persistence.enabled: true` that grant is not emitted at
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
| its own identity Secret | get, update (namespaced Role, `resourceNames`) | Persist the in-cluster-generated key and certificate across rescheduling (ADR 0008). Helm pre-creates the Secret; the agent owns only its content. No `create` (cannot be name-scoped), no `list`/`watch`/`delete` — the agent can neither enumerate nor touch any other Secret. Not emitted when `persistence.enabled: true` |

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
| Pod pprof endpoints | controller → pods | Pull `/debug/pprof/profile` and `/debug/pprof/heap` from Go services that already expose them | Only pods that pass the workload filters are probed; ports taken from `containerPorts`, never blind scans. `pprof`/`ebpf` profiles only |
| kubelet stats, proxied | controller → API server | Poll `/stats/summary` and `/metrics/cadvisor` on every kubelet for usage counters (ADR 0006) | Goes through the API server proxy (`nodes/proxy`, §4) — the agent opens **no direct connection to kubelets**. A direct-kubelet transport for very large clusters would be a documented change here, not a silent one |
| Backend egress | controller → one fixed domain | Ship aggregated rollups and filtered profiles | mTLS, pinned domain. The only cross-boundary connection in the system. A NetworkPolicy restricting controller egress to this domain plus in-cluster targets is shipped with the chart |
| Node → controller reports | node → controller, in-cluster only | Deliver on-node Go build-info findings (and, later, profiles) for aggregation (ADR 0010), and ask which pods on this node passed your filters (ADR 0015) | Plain HTTP on the cluster network; the node always initiates and authenticates with a projected controller-audience ServiceAccount token, validated locally (no `TokenReview`). The report endpoints answer ack/error only; the scope and profiling-target queries answer with in-cluster identifiers derived from your own filters, which can only narrow what the node does and never widen it. The receiver is not reachable from outside the cluster, and a shipped NetworkPolicy restricts it to the DaemonSet. The DaemonSet has **no** external egress. Honest disclosure: the token and the (already-filtered) facts travel in cleartext in-cluster; a party that can already sniff pod traffic could replay the short-lived token to post fabricated inventory facts for this one cluster — bounded, one-way, no monetary or read capability (ADR 0010) |

---

## 6. Agent identity and credential lifecycle

The controller authenticates to the backend with a per-cluster client
certificate (mTLS). The private key is generated inside your cluster and never
leaves it. Provisioning follows a two-tier scheme, so no long-lived shared
secret is ever distributed across clusters:

```
Org API key           — long-lived, revocable; lives in your automation
                        (Terraform / CI / secret manager). NEVER enters a cluster.
  └─ issues per-cluster enrollment tokens
Enrollment token      — short TTL, bound to one pre-created cluster record;
                        the only credential that enters the cluster.
  └─ exchanged for a client certificate
Client certificate    — key pair generated by the agent in-cluster; the issued
                        certificate carries the cluster identity.
```

Blast radius: a leaked enrollment token exposes one cluster for the duration
of its TTL; a leaked org API key is revoked in one action without affecting
any running agent, because agents run on certificates, not on the key.

### Enrollment flow

1. A cluster record is created in the backend (UI, CLI, or Terraform),
   authorized by the org API key. This returns an enrollment token with a
   short TTL, reusable within that TTL (tolerates restarts during rollout).
2. The token is delivered via Helm: either inline (`enrollment.token`) or as
   a reference to a pre-created Secret (`enrollment.existingSecret`). The
   latter is the intended path for GitOps / External Secrets setups, so
   tokens never appear in git.
3. On first start the controller generates a key pair locally and submits a
   CSR to the backend enrollment endpoint, authenticated by the token. The
   request includes a cluster fingerprint (the UID of the `kube-system`
   namespace) so the backend detects a token accidentally applied to a
   different cluster instead of silently mixing data.
4. The backend signs the certificate with the cluster identity embedded. All
   further communication is mTLS; the enrollment token is not used again.

Enrollment is an outbound request initiated by the agent — it does not create
a control channel (principle 2 in [§1](#1-design-principles)).

### Storage

The controller runs as a single-replica StatefulSet with a small local
volume — an `emptyDir` by default; `persistence.enabled: true` swaps in a
PersistentVolume (`volumeClaimTemplates`, ~2Gi) for installations that want
unacknowledged data to survive pod rescheduling (ADR 0007). The volume
holds exactly two things:

1. **Collector bookkeeping** — watermarks of closed/acknowledged hours,
   the workload registry with metadata content hashes, profiling rotation
   state. No collected data, only accounting.
2. **The spool of unshipped payload batches** — rollups, metadata,
   coverage reports, and allow-listed profiles, held until delivery is
   acknowledged and deleted immediately after (with a maximum-age cap, so
   an extended outage cannot fill the volume). Only data that has already
   passed the filters — i.e. data approved to leave the cluster — is ever
   written to the volume.

**Credentials** — the in-cluster-generated private key and client
certificate — live in the agent's pre-created identity Secret (ADR 0008),
so identity survives rescheduling and node loss without any volume. This
is the one write grant in the product (see the RBAC table above). Honest
consequences: the key exists in etcd and in any backup that includes
Secrets; whoever reads it can impersonate this cluster's telemetry stream —
data poisoning of this one cluster, nothing more (the protocol is one-way
and carries no read or control capability), recovered by revoke +
re-enroll. With `persistence.enabled: true` credentials move to the PVC
instead and the Secret grant disappears.

- The volume exists for continuity, not as a source of truth. Everything on
  it is reconstructible (history lives in the backend, enrollment can be
  repeated), so it requires no backups.
- Configuration is never cached on the volume. The agent reads its filters
  from the ConfigMap on every start; if the configuration is unavailable,
  the agent does not collect. A stale filter configuration cannot resurrect
  from disk by construction.
- Encryption at rest is delegated to your storage layer: point
  `controller.persistence.storageClass` at an encrypted StorageClass.
- With the default `emptyDir`, a pod rescheduled to another node keeps its
  identity (it lives in the Secret, ADR 0008) and loses only the
  unacknowledged spool: bounded by the minute snapshot cadence in normal
  operation, and by the outage span if the backend was unreachable
  (ADR 0007) — coverage reporting makes the gap visible. Re-enrollment is
  needed only if the identity Secret itself is deleted or the certificate
  is revoked.

### Rotation, revocation, recovery

- Certificates are short-lived; the agent renews them itself over the
  existing mTLS channel before expiry. No external trigger is involved.
- The backend can revoke any cluster's certificate at any time. Org API keys
  are revocable independently of running agents.
- If key material is lost (the identity Secret — or, in persistence mode,
  the PersistentVolume — is deleted), a new
  enrollment token is issued for the **same** cluster record ("re-enroll");
  the previous certificate is revoked and the cluster's data history stays
  continuous.
- With an expired or revoked certificate and no valid enrollment token, the
  agent degrades to local-only operation (keeps collecting into its rollups
  and ring buffer) and logs the condition — it does not crash-loop and does
  not retry-storm the backend.

### Server trust

The agent pins the backend's private CA rather than the system trust store,
so a TLS-intercepting corporate proxy cannot impersonate the backend. Note
that such a proxy also cannot pass the mTLS session through: in environments
with egress interception, the backend domain needs a bypass rule.

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

**What decides who gets profiled.** The node asks the controller which of the
node's *already-permitted* workloads rank highest by consumption, but the
eligible set and every ceiling (which namespaces/modules may be profiled at all,
the capture duration, the frequency, and the overhead cap) live only in the
node's ConfigMap. The controller's answer can prioritize within that permission
but can never widen it — no reply turns profiling on for a workload your
configuration did not already allow (ADR 0011 §3).

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

The declared shape of your workloads leaves the cluster as two payloads, both
of them snapshots of current state that replace their predecessor rather than
accumulating history:

- **`workload_metadata`** — per (namespace, workload, container, image digest):
  the image reference, declared requests and limits, and declared container
  ports, plus a `pod` block with the QoS class, how many replicas currently
  carry that build, and the counts of those replicas per pod phase and per node
  name. A pod with several containers appears as one record per container, each
  repeating the same `pod` block. It contains no pod names: replicas are
  counted, not listed.
- **`node_metadata`** — per node: name, size, instance type, capacity type,
  zone, region, kernel version, CPU architecture.

`workload_metadata`'s `pod` block also counts the replicas that are *not* on a
node, by the reason the scheduler gave (`Unschedulable`, `SchedulingGated`,
`SchedulerError`, or `other`). The count of unplaced replicas was always visible
as the gap between `replicas` and the `nodes` breakdown; this says why, without
naming any pod (ADR 0021).

Both are limited to what passes your filters — a pod excluded by a namespace
filter or an opt-out annotation is absent from the snapshot entirely, and its
identity never appears in either payload. Every payload declares its
provenance (`source`), so declared values can never be silently merged with
measured or sampled ones.

Both also carry `captured_at`, the instant the snapshot was assembled (ADR
0017), as does the Go inventory of §8. It is a timestamp of the agent's own
work, not of anything in your cluster; its purpose is that a stale snapshot can
be recognized as stale rather than assumed current.

### Object history, and the one place pods are named (ADR 0020)

Two payloads report what the cluster's own object status records about a
container's history. Both are `journal` provenance, and both name the **pod**:

- **`oom_kill`** — one event per observed out-of-memory kill: namespace, pod,
  container, workload, when it finished, the exit code, the restart count at the
  time, and the memory limit the container declared.
- **`container_restarts`** — per (namespace, pod, container) and hour-aligned
  window: how many times the container restarted, a breakdown of those restarts
  by termination reason, how many restarts had no reason visible, and the most
  recent exit code.
- **`pod_disruptions`** — per hour-aligned window, one record per pod the
  *cluster* removed rather than the workload itself: preempted to make room for
  higher priority, evicted by a node under pressure, drained by a taint, or
  removed through the eviction API. Each record carries the namespace, pod,
  workload, the node it was taken from, the reason, and the instant Kubernetes
  recorded. A pod that failed on its own — its container exited non-zero — is
  not in here.

**Why the pod is named here and nowhere else.** Every other payload counts
replicas instead of listing them, because a workload-level number answers the
question. A crash loop is different: "one of your twelve replicas restarts every
thirty seconds" is not actionable without knowing which one. The pod name is the
smallest identifier that makes the record useful, and it is a name your cluster
generated, never workload content.

If you would rather it did not leave, the controls are the ones in
[§11](#11-your-controls): a namespace filter, a label selector, or the
`rebuildstack.co/collect: "false"` annotation. An excluded pod produces no
journal record at all — the exclusion applies before any of this is formed, and
also removes what was already collected.

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

The purpose is honesty about gaps. Without it, a container that was never
throttled and a container whose node could not be scraped produce identical
records, and an analysis outside the cluster cannot tell them apart — the
distinction is unrecoverable once the moment has passed. No pod, node, or
workload is named in this data; the failure counts are cluster-wide totals.

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
  digest, time window, `source=ebpf`).
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
  truncated, since every allowed setting holds a short bounded token.

  `vcs.revision` is a commit identifier from your repository. It reveals no
  source and is meaningless without access to that repository, and it is
  frequently absent: the Go toolchain records it only when the build had a VCS
  working tree, which containerized builds often do not. Its absence is normal
  and is not treated as a gap.
- **Dropped — infrastructure.** A main module on the built-in deny-list
  (`k8s.io/`, `sigs.k8s.io/`, the container runtime, the CNI, `go.etcd.io/`,
  `github.com/coredns/`, `github.com/prometheus/`, `github.com/grafana/`, … and
  this agent's own `github.com/RebuildStackCo/`) is dropped on the node. Its
  identity — module path, pod UID, container ID — is never recorded, only
  counted.
- **Counted, never identified.** Four aggregate counters describe everything
  not kept: processes scanned, Go binaries found, filtered as infrastructure,
  and unreadable (a real executable with no recoverable Go build info — a
  non-Go program, or a Go binary whose build info was removed). No identity of
  a filtered or unreadable process leaves as anything but a number
  (invariant 6).

The controller joins the kept facts against its workload inventory (ADR 0010) —
pod UID → workload, container ID → container name and the image digest it
already collects — into two payloads:

- **`go_inventory`** — one record per (namespace, workload, container) carrying
  the Go version, main module path, image digest, and PGO flag. Replicas and
  nodes running the same build collapse to one record. A record exists only
  while its workload does: on every flush the inventory drops whatever the
  agent's filtered pod index no longer holds, so a deleted or opted-out
  workload leaves the payload rather than lingering as a last known state
  (ADR 0018). It also carries a `coverage` block: how many of the cluster's
  nodes have delivered a report, and how many facts were received, joined, and
  not joined. Those are cluster-wide counts, and they name nothing.
- **`go_build`** — what one build is made of and how it was built, keyed by its
  image digest (ADR 0017, ADR 0019): the Go version, the main module, the
  dependency module paths, and the allow-listed build settings above. One
  payload per distinct build, sent once, joined back to workloads through the
  image digest in `go_inventory`. **Module paths only, never versions**: the
  agent does not collect the version of any dependency, so this is not, and
  cannot be used as, a vulnerability-scanning feed.

A fact whose pod or container the controller cannot resolve (informer lag, or a
filtered pod) is counted as *unjoined* and dropped — its pod UID, container ID,
and module path never appear, only the count. A fact that resolves to a
container with no image digest yet is counted as *undigested*: its record is
kept, its build payload is not, because there is no build to key it to. This is
the same "medium sensitivity" workload-metadata class as the rest of §8: module
and version strings, never source, environment, or arguments.

### What the agent reports about your filters

The rule: full information about what is collected, only aggregate
information about what is not.

- **Coverage counts per filter type** are sent (e.g. "82 workloads
  discovered, 64 collected, 12 excluded by namespace filter, 6 by
  annotation") so that reports can state their coverage honestly.
- A **fingerprint (hash) of the effective filter configuration** and the
  time it last changed accompany shipped data, so any upload can be
  attributed to the configuration active at that moment. The fingerprint
  reveals nothing about the configuration's content.
- **Names of excluded objects are never transmitted** — not namespaces, not
  workloads, not deny-listed module paths. The only filter content that
  leaves the cluster is the profile module-path allow-list, which is already
  self-evident from the profiles it admits.
- Node-level totals (allocatable, aggregate node usage) are collected in
  cluster-wide mode regardless of workload filters. They are not
  attributable to individual workloads and are required to reconcile total
  cluster cost against your invoice.

### Transport

- Controller → backend over mTLS to a single fixed domain.
- Payload: hourly rollup histograms, workload metadata as described above, the
  Go inventory (above), and allow-listed symbolized profiles. Nothing else.

---

## 9. What the agent does NOT request

Stated explicitly so it does not have to be asked:

- No `secrets`, no `configmaps`.
- No `pods/exec`, `pods/attach`, `pods/portforward`.
- No write verbs of any kind — the agent is physically unable to change
  cluster state through its ServiceAccount.
- No cloud provider credentials, IAM roles, or billing API access. Node
  pricing uses a static price table plus your stated discount.
- No external egress from nodes — only the controller crosses the boundary.
- No access to your monitoring stack. The agent does not query Prometheus,
  Thanos, Mimir, or any other metrics store — not as an option and not to
  backfill history at install time (ADR 0016, [§10.1](#101-the-agent-knows-nothing-about-the-time-before-it-was-installed)).
  It measures what it measures itself.
- No API access from the node role at all — its ServiceAccount holds zero RBAC,
  and the only token it mounts is audience-bound to the controller, which the
  API server rejects ([§7](#7-node-privileges), ADR 0009/0010). The default API
  token is never mounted, and the node role never calls the Kubernetes API.
- No dynamic configuration from the backend — the agent cannot be
  reconfigured remotely.

---

## 10. Known limitations and honest disclosures

### 10.1 The agent knows nothing about the time before it was installed

Every measurement the agent reports it made itself, from the kubelet counters
of ADR 0006. It does **not** read your Prometheus — not as an option, not for a
one-time backfill (ADR 0016) — so a cluster installed today has no usage data
for yesterday, and a finding that needs a long observation window has to wait
for that window.

This is a deliberate refusal, not an unbuilt feature. A Prometheus response
carries no record of what shaped it: recording rules, downsampling and
`metric_relabel_configs` all alter a series and leave no trace in the answer, so
the agent could not tell a raw series from a five-minute average, nor a workload
that did not exist from one a relabel rule discarded. Everything else in this
agent's protocol declares its provenance and how completely it was observed
([§8](#8-data-collected-and-data-leaving-the-cluster)); imported series could
declare neither, and would end up joined with exact data under the same keys.
There is also a tenancy asymmetry: a Prometheus endpoint typically serves all
namespaces regardless of the caller's Kubernetes permissions, so your namespace
filters would be enforced only inside the agent on that path, while every other
path enforces them at collection.

What does not need history still works on day one: declared requests and limits,
QoS, topology and placement, probe configuration, and the object journal
(conditions, restart counts, ReplicaSet revisions, Job timings) are all true at
install time. What genuinely needs observation reports how long it has been
observing, so its weight is visible rather than assumed.

### 10.2 Node-level visibility cannot be namespace-scoped

With `hostPID` and eBPF, the node component technically observes all processes
on a node, including pods from namespaces outside your filters — this is a
property of node-level profiling, not of this agent. The mitigation is the
collection-stage filter: samples from processes whose module path or namespace
is not allow-listed are discarded on the node, before transport to the
controller. If this is not acceptable, use the `pprof` or `metrics-only`
profile, where no node component exists.

**How the node knows your namespaces** (ADR 0015). The node component holds no
Kubernetes API access at all, and a process's cgroup tells it a pod UID, never a
namespace — so it cannot evaluate your filters itself. On each pass it asks the
controller, over the same in-cluster, node-initiated channel it uses for
everything else, which pods on this node passed your filters, and scans only
those. Two properties follow:

- **A process outside that set has its executable never opened.** The scope is
  checked on the cgroup first, so no module path is ever extracted for a pod you
  excluded — not collected, as opposed to collected and discarded. Only an
  aggregate count of skipped processes is reported.
- **It fails closed.** A node that cannot reach the controller, or that is
  deployed without the scope endpoint, scans nothing at all rather than falling
  back to scanning everything. An outage costs you one scan pass, never a
  widening of what is collected.

A process belonging to no pod — the kubelet, a systemd unit, anything outside
the pod hierarchy — is never in scope, because it is in no namespace and so no
namespace filter of yours can permit it.

### 10.3 pprof endpoint probing

Locating pprof endpoints means the controller makes HTTP requests to pods,
which network monitoring may flag as internal scanning. Probing is restricted
to workloads that pass your filters and to ports declared in `containerPorts`.
Coordinate with your security team before enabling the `pprof` profile; the
`metrics-only` profile performs no probing.

---

## 11. Agent self-reporting

- **Startup self-audit:** the agent enumerates its effective permissions via
  `SelfSubjectRulesReview` and logs exactly what it can and cannot see. A 403
  results in graceful degradation and a log line, never a crash-loop or retry
  storm.
- **Coverage report:** every run reports how many workloads were discovered,
  how many were collected, and how many were excluded by which filter
  (namespace filter, label selector, opt-out annotation, module path). This
  makes the filtering behavior verifiable from output, not just from
  configuration.

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
