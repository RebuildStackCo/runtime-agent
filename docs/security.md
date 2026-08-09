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
runtime-agent controller   # StatefulSet, 1 replica — talks to k8s API, Prometheus,
                           #   pod pprof endpoints; sole egress point
runtime-agent node         # DaemonSet (optional)   — eBPF profiling and Go binary
                           #   detection on the node; talks only to the controller
```

- The node role sends data to the controller over the cluster network only.
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
| `metrics-only` | controller | none | Cost and efficiency findings from usage metrics (kubelet counters via the API server, ADR 0006; Prometheus as a side-channel for history) |
| `pprof` | controller | none | Above + CPU/heap profiles pulled from services that already expose `/debug/pprof` |
| `ebpf` | controller + node DaemonSet | see [§7](#7-node-privileges-ebpf-profile-only) | Above + CPU profiles for services without pprof endpoints |

---

## 4. Kubernetes API access (RBAC)

All rules are `get`, `list`, `watch` only. There are no `create`, `update`,
`patch`, `delete`, or `deletecollection` verbs anywhere in the chart.

| Resource | Verbs | Why |
|---|---|---|
| `pods` | get/list/watch | Map containers to workloads; read `requests`/`limits` from spec; detect OOM kills via `status.containerStatuses[].lastState.terminated.reason == "OOMKilled"`; read `containerPorts` to locate pprof endpoints |
| `replicasets`, `deployments`, `statefulsets`, `daemonsets`, `jobs`, `cronjobs` | get/list/watch | Resolve the `ownerReferences` chain (Pod → ReplicaSet → Deployment) so findings are aggregated per workload, not per pod |
| `nodes` | get/list/watch | `allocatable`/`capacity` for node idle computation; labels `node.kubernetes.io/instance-type` and capacity-type (spot/on-demand) for the cost model |
| `namespaces` | get/list/watch | Evaluate namespace allow/deny filters and the opt-out annotation |
| `nodes/proxy` | get | Poll each kubelet's `/stats/summary` and `/metrics/cadvisor` through the API server for usage counters: CPU, memory working set, CFS throttling, PSI where exposed (ADR 0006). **Honest disclosure:** this verb technically permits any kubelet GET endpoint through the API server, including node logs. The agent calls exactly the two stats paths above — auditable, since all kubelet access lives in a single poll loop |

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
| Prometheus HTTP API (`query`, `query_range`) | controller → your Prometheus | Historical metrics over the retention window; enables a report on installation day | Read-only query API. Endpoint set in Helm values. See [§10.1](#101-prometheus-is-a-side-channel) |
| Pod pprof endpoints | controller → pods | Pull `/debug/pprof/profile` and `/debug/pprof/heap` from Go services that already expose them | Only pods that pass the workload filters are probed; ports taken from `containerPorts`, never blind scans. `pprof`/`ebpf` profiles only |
| kubelet stats, proxied | controller → API server | Poll `/stats/summary` and `/metrics/cadvisor` on every kubelet for usage counters (ADR 0006) | Goes through the API server proxy (`nodes/proxy`, §4) — the agent opens **no direct connection to kubelets**. A direct-kubelet transport for very large clusters would be a documented change here, not a silent one |
| Backend egress | controller → one fixed domain | Ship aggregated rollups and filtered profiles | mTLS, pinned domain. The only cross-boundary connection in the system. A NetworkPolicy restricting controller egress to this domain plus in-cluster targets is shipped with the chart |
| Node role | node → controller only | Deliver profiles and binary metadata for aggregation | The DaemonSet has **no** external egress |

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
unacknowledged data to survive pod rescheduling (ADR 0007). Either way the
volume holds exactly three things:

1. **Credentials** — the client key (mode 0600, dedicated `fsGroup`) and
   certificate.
2. **Collector bookkeeping** — watermarks of closed/acknowledged hours,
   the workload registry with metadata content hashes, profiling rotation
   state. No collected data, only accounting.
3. **The spool of unshipped payload batches** — rollups, metadata,
   coverage reports, and allow-listed profiles, held until delivery is
   acknowledged and deleted immediately after (with a maximum-age cap, so
   an extended outage cannot fill the volume). Only data that has already
   passed the filters — i.e. data approved to leave the cluster — is ever
   written to the volume.

The key and certificate are deliberately **not** written to a Kubernetes
Secret: the agent holds no write access to Secrets, and that does not change
for its own credentials.

- The volume exists for continuity, not as a source of truth. Everything on
  it is reconstructible (history lives in the backend, enrollment can be
  repeated), so it requires no backups.
- Configuration is never cached on the volume. The agent reads its filters
  from the ConfigMap on every start; if the configuration is unavailable,
  the agent does not collect. A stale filter configuration cannot resurrect
  from disk by construction.
- Encryption at rest is delegated to your storage layer: point
  `controller.persistence.storageClass` at an encrypted StorageClass.
- With the default `emptyDir`, a pod rescheduled to another node loses its
  credentials and any unacknowledged spool. The agent re-enrolls
  automatically as long as the enrollment token in the referenced Secret is
  still within its TTL; data loss is bounded by the minute snapshot cadence
  in normal operation, and by the outage span if the backend was
  unreachable (ADR 0007) — coverage reporting makes the gap visible.

### Rotation, revocation, recovery

- Certificates are short-lived; the agent renews them itself over the
  existing mTLS channel before expiry. No external trigger is involved.
- The backend can revoke any cluster's certificate at any time. Org API keys
  are revocable independently of running agents.
- If key material is lost (e.g. the PersistentVolume is deleted), a new
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

## 7. Node privileges (`ebpf` profile only)

The DaemonSet is not installed at all in `metrics-only` and `pprof` profiles,
and this entire section does not apply.

| Privilege | Why |
|---|---|
| `hostPID: true` | Read `/proc/<pid>/exe` of host processes to extract Go `buildinfo` (Go version, module path, whether `-pgo` was applied) without entering containers |
| `CAP_BPF` + `CAP_PERFMON` | Load eBPF programs and perform perf sampling for CPU profiles (kernel 5.8+) |
| `CAP_SYS_PTRACE` — **TBD** | Reading another process's `/proc/<pid>/exe` is subject to the kernel's ptrace access check; we are verifying whether this capability is required. This document will state the final, minimal set |
| hostPath mounts — **TBD (exact list)** | Required by the embedded OpenTelemetry eBPF profiler for stack unwinding and on-node symbolization (expected: `/proc`, kernel BTF; final list will be pinned here) |

Compensating controls, all set in the shipped chart:

- `privileged: false` — the pod never requests full privileges. On kernels
  older than 5.8, where `CAP_BPF`/`CAP_PERFMON` do not exist, the `ebpf`
  profile is unsupported rather than silently escalated.
- Read-only root filesystem.
- Seccomp profile applied.
- Capabilities dropped to the minimal set listed above; nothing else is added.

**Symbolization happens on the node.** Your binaries are never uploaded
anywhere. Stack addresses are resolved to function names locally, and only
symbolized, filtered profiles (see [§8](#8-data-collected-and-data-leaving-the-cluster))
leave the node — and only towards the in-cluster controller.

---

## 8. Data collected and data leaving the cluster

This is usually the most important section for review. Data falls into three
classes with different sensitivity and different default policies:

| Data class | Examples | Sensitivity | Default policy |
|---|---|---|---|
| Resource metrics | CPU usage, working set, throttling ratios, requests/limits, node allocatable | Low (numbers) | Collected for everything visible, minus the infrastructure deny-list and your filters |
| Workload metadata | Workload names, namespaces, image digests, Go version, module path | Medium | Same as metrics |
| Profiles (stack traces) | Function names, call graphs | **High** — function names reveal the structure of your code | **Explicit allow-list only.** A stack trace never leaves the cluster unless its module path is on the allow-list you configured |

### Field minimization

- The agent **never reads Secrets or ConfigMaps** (no RBAC for them exists).
- From pod specs, the agent keeps only `metadata`, `spec.containers[].resources`,
  `spec.containers[].ports`, `ownerReferences`, and `status`. It explicitly
  discards `env`, `args`, and `command` before anything is stored or
  transported, because these fields frequently contain inline credentials.

### Profile filtering (applies at collection, not at egress)

- Stack frames are filtered by Go module path against your allow-list.
  Frames from paths not on the list (Kubernetes components, service meshes,
  observability stacks, this agent itself) are dropped.
- Aggregated metric rollups are hourly mergeable histograms per workload —
  no raw per-second samples are shipped.
- Raw, unfiltered samples exist only in a 24–48h ring buffer on an ephemeral
  `emptyDir` volume, for debugging and flame graphs. They are never
  transported and never written to the persistent volume — unfiltered stack
  traces do not survive pod deletion. Profiles that passed the allow-list
  (i.e. are approved to leave the cluster) may sit in the spool on the
  persistent volume until their delivery is acknowledged, and are deleted
  right after.

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
- Payload: hourly rollup histograms, workload metadata as described above,
  and allow-listed symbolized profiles. Nothing else.

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
- No dynamic configuration from the backend — the agent cannot be
  reconfigured remotely.

---

## 10. Known limitations and honest disclosures

### 10.1 Prometheus is a side channel

Kubernetes RBAC does not govern what the agent can read from your Prometheus:
a Prometheus endpoint typically serves metrics for all namespaces regardless
of the caller's Kubernetes permissions. If you install namespace-scoped, be
aware that the metrics path is only constrained by the agent's own namespace
filters (soft boundary, auditable in the ConfigMap) unless your Prometheus
deployment enforces tenancy itself (e.g. a tenant-scoped query frontend). If
you have a scoped query endpoint, point the agent at it.

### 10.2 Node-level visibility cannot be namespace-scoped

With `hostPID` and eBPF, the node component technically observes all processes
on a node, including pods from namespaces outside your filters — this is a
property of node-level profiling, not of this agent. The mitigation is the
collection-stage filter: samples from processes whose module path or namespace
is not allow-listed are discarded on the node, before transport to the
controller. If this is not acceptable, use the `pprof` or `metrics-only`
profile, where no node component exists.

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

The exclusion applies at the collection stage.

---

## 12. Reporting a vulnerability

Contact: **TBD** (security contact will be published before the first external
deployment). Please do not open public issues for suspected vulnerabilities.
