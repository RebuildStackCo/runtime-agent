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
   and decided in [ADR 0008](adr/0008-identity-in-secret.md). **[planned]** — no
   manifest emits that Role today.

   The verb list is the claim; "therefore it cannot change anything" is not, and
   this document used to draw that inference. One grant reaches past it:
   `get` on `nodes/proxy` is what the kubelet answers `exec` on, and §4 and
   [§9](#9-what-the-agent-does-not-request) now say so. What bounds it is a
   property of this code rather than of the grant
   ([ADR 0039](adr/0039-stated-limits-are-measured-limits.md)).
2. **One-way data flow.** The agent pushes data out; there is no command or
   control channel from the backend to the agent. The backend cannot instruct
   the agent to do anything. All behavior is defined by the in-cluster
   configuration (Helm values → ConfigMap), which you own, version, and audit.
3. **Every access maps to a feature.** Disabling a feature removes the
   corresponding access. Nothing is requested "just in case."
4. **Filter as early as possible.** "Not collected" is better than "collected
   and discarded," which is better than "collected and not sent." For each
   filter, this document states the stage at which it applies — including where
   the principle is not achieved. The binary scanner reaches it: a pod's cgroup
   decides scope before its executable is opened, so an excluded pod's build
   info is never extracted. The eBPF profiler does not and cannot: perf sampling
   attaches to the node, not to a process set, so its frames are captured first
   and filtered after ([§7.2](#72-the-ebpf-cpu-profiler-opt-in-ebpf-profile-adr-0011),
   [§10.2](#102-node-level-visibility-cannot-be-namespace-scoped)).
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
  identity Secret and the one holder of the cluster's backend identity. It is a
  Deployment with `strategy: Recreate` — one replica, and the old pod stops
  before the new one starts. Not a StatefulSet: it claims no volume and no
  stable name, and a StatefulSet would hold a replacement back while a node is
  unreachable, stopping collection cluster-wide (ADR 0026).
- The node role sends data to the controller over the cluster network only.
- The host boundary runs between the two roles, and only the node role crosses
  it. The controller shares no namespace with the node it lands on, mounts no
  host path, and adds no capability — asserted per rendered pod in
  `internal/chartrender/guardrail_test.go`, so §7's blast-radius paragraphs stay
  paragraphs about the DaemonSet.
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
| `inventory` | controller + node DaemonSet | host PID, `CAP_SYS_PTRACE` (see [§7.1](#71-the-go-binary-scanner-the-current-node-role)) | Above + which of your binaries are Go, their version and module path (ADR 0009) |
| `pprof` **[planned]** | controller | none | Above + CPU/heap profiles pulled from services that already expose `/debug/pprof`. No pprof puller exists yet; nothing in the agent opens a `/debug/pprof` endpoint today, and the chart refuses to install this profile rather than pretend |
| `ebpf` | controller + node DaemonSet | adds `CAP_BPF`, `CAP_PERFMON` (see [§7.2](#72-the-ebpf-cpu-profiler-opt-in-ebpf-profile-adr-0011)) | Above + CPU profiles for services without pprof endpoints |

The profile is the whole of the choice: `helm install --set profile=...`. There
is no second setting that half-enables a component, and each profile is a
superset of the one above it (ADR 0036).

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
(ADR 0008) — **[planned]**, and the only row of this table that is. Nothing else
anywhere carries `create`, `update`, `patch`, `delete`, or `deletecollection`.

That one grant is unconditional once shipping exists, and there is no setting
that removes it (ADR 0026). What removes it is not shipping: an installation
that sends nothing to a backend holds no identity, so there is nothing to
write and the grant is not emitted.

The table below is the **controller's** access. The node role (the DaemonSet,
[§7](#7-node-privileges)) holds **no** RBAC at all — its ServiceAccount is
bound to nothing and its token is not mounted — so it appears in no row here.

| Resource | Verbs | Why |
|---|---|---|
| `pods` | get/list/watch | Map containers to workloads; read `requests`/`limits` from spec; detect OOM kills and count restarts from `status.containerStatuses[]` (`restartCount`, and the reason and exit code of `lastState.terminated`); read the `PodScheduled` and `DisruptionTarget` conditions to report why a replica is not running and when the cluster took one away; read `containerPorts` to locate pprof endpoints |
| `replicasets`, `deployments`, `statefulsets`, `daemonsets`, `jobs`, `cronjobs` | get/list/watch | Resolve the `ownerReferences` chain (Pod → ReplicaSet → Deployment) so findings are aggregated per workload, not per pod; read each workload's own annotations to honor an opt-out written there ([ADR 0028](adr/0028-workload-level-opt-out.md)); and read finished Jobs' timings and outcomes for `job_runs` ([ADR 0029](adr/0029-job-runs.md)) |
| `nodes` | get/list/watch | `allocatable`/`capacity` for node idle computation; labels `node.kubernetes.io/instance-type` and capacity-type (spot/on-demand) for the cost model; `status.nodeInfo.kernelVersion` to report whether nodes meet the kernel floor for the eBPF profile (`CAP_BPF` requires kernel 5.8+, see [§7](#7-node-privileges)) |
| `namespaces` | get/list/watch | Evaluate namespace allow/deny filters and the opt-out annotation |
| `services` | get/list/watch | Report that traffic is routed to a workload at all ([ADR 0048](adr/0048-findings-name-the-fields-they-need.md)): one replica is a batch size or an outage depending on whether anything sends it requests, and a declared `containerPort` is an announcement rather than a route. The selector is resolved against pods that passed your filters and never reported as such. Addresses are not read — a ClusterIP is compared against `None` to tell a headless Service apart, and nothing else. Also read: `spec.trafficDistribution` and the topology-mode annotation, which say whether you asked for traffic to stay inside a zone |
| `endpointslices` (`discovery.k8s.io`) | get/list/watch | Count, per Service, how many ready endpoints sit in each zone and how many carry the routing hint the cluster decides to give ([ADR 0051](adr/0051-topology-routing-is-asked-and-not-granted.md)). Setting `trafficDistribution` is a request the EndpointSlice controller may silently decline, and nothing on the Service says which happened. **Endpoint addresses are pod IPs and are never read**: they, the `targetRef` naming the pod, its node and its hostname are removed as the object enters the cache, so the agent never holds them (ADR 0046). Only counts leave, and no endpoint is identified |
| `poddisruptionbudgets`, `horizontalpodautoscalers` | get/list/watch | Report what bounds a workload from outside its own spec ([ADR 0032](adr/0032-cluster-policy.md)): a budget can forbid eviction outright, so a node covered by one cannot be drained at all; an autoscaler targeting CPU *utilization* targets a percentage of the request, so the request cannot be read safely without it. A budget's selector is resolved against pods that passed your filters and never reported as such |
| `limitranges`, `resourcequotas` | get/list/watch | Distinguish a request a team chose from one the namespace supplied on their behalf, and report the ceiling a namespace sits under and how much of it is spent (ADR 0032). Read only for namespaces that pass your filters |
| `persistentvolumeclaims`, `storageclasses` | get/list/watch | A bound claim on a zonal volume pins its pod to that zone, which no field of the pod spec states (ADR 0032). A StorageClass's `parameters` are **not** read — that is the one field here carrying provider configuration (endpoints, key identifiers); what is read is how the class behaves: provisioner, reclaim policy, `volumeBindingMode`, expansion |
| `priorityclasses` | get/list/watch | The value and preemption policy behind the `priorityClassName` already collected in the placement block ([ADR 0031](adr/0031-placement-constraints-are-reduced.md)), and the explanation for preemptions already reported in `pod_disruptions` |
| `nodes/proxy` | get | Poll each kubelet's `/stats/summary` and `/metrics/cadvisor` through the API server for usage counters: CPU, memory working set, CFS throttling, PSI where exposed (ADR 0006). **Honest disclosure — this is the widest grant in the table, and `get` understates it.** The kubelet serves `/exec/{ns}/{pod}/{container}`, `/attach/…` and `/portForward/…` over **GET**, so this verb permits command execution in any container in the cluster, as well as `/containerLogs/…`, which routinely carries credentials. It is the grant that makes "the agent cannot change your workloads" untrue as an inference from the verb list ([§1](#1-design-principles), [ADR 0039](adr/0039-stated-limits-are-measured-limits.md)). What bounds it is the code, not the grant, and the bound is auditable: exactly two call sites exist in the whole repository, both `GET`, both the stats paths above, both in one poll loop. Withholding this grant costs usage collection: the agent keeps running, but — unlike the policy caches described below — the absence is a log line per node per poll rather than a declared `unavailable_sources` entry, so a payload with no usage in it does not say why |
| its own identity Secret **[planned]** | get, update (namespaced Role, `resourceNames`) | Persist the in-cluster-generated key and certificate across rescheduling (ADR 0008). Helm pre-creates the Secret; the agent owns only its content. No `create` (cannot be name-scoped), no `list`/`watch`/`delete` — the agent can neither enumerate nor touch any other Secret |

**The controller runs as a non-root user.** It reads the API, polls kubelets
through it, and writes payload files to an `emptyDir` — it mounts no host path
and opens no other process's `/proc`, so it needs no privilege and takes none
(ADR 0037). It declares `runAsNonRoot`, so an image rebuilt without its non-root
user fails to start rather than quietly regaining root. The one component that
does run as root is the node role, in
[§7.1](#71-the-go-binary-scanner-the-current-node-role).

**Declining a policy grant degrades one payload, not the agent.** The caches
behind `services`, `poddisruptionbudgets`, `horizontalpodautoscalers`,
`limitranges`, `resourcequotas`, `persistentvolumeclaims`, `priorityclasses` and
`storageclasses` gate nothing else, so withholding one costs the corresponding
facts and nothing more. The payload then names the source it could not read in
`unavailable_sources`, so the absence is legible rather than indistinguishable
from "your cluster has none of those"
([ADR 0033](adr/0033-policy-sources-degrade.md)). Granting it later takes effect
at the next flush, without a restart. This holds for a permission narrowed later,
not only one never granted
([ADR 0035](adr/0035-watch-failures-are-noticed.md)); detection lags the change
by up to ten minutes, because an established watch is not re-authorized until the
client re-opens it. What the payload names is the resource class, never an
object.

**Withdrawing a grant the agent cannot work without stops the agent, visibly.**
`pods`, `namespaces`, `nodes` and the owner-chain resources are what every
payload is assembled from, so there is no single payload left to degrade. If one
becomes unreadable the agent exits with a message naming the resource and its pod
enters `CrashLoopBackOff`. It does not keep answering from a frozen cache, and it
does not sit silently waiting (ADR 0035).

**Authenticating node reports adds no RBAC.** The controller verifies each node's
projected token against the cluster's published JWKS **locally**. It does not
call `TokenReview`, so no `create` on `tokenreviews` is added. The same token
establishes *which node* is calling: a token projected into a pod is bound to
that pod, and the kubelet writes the pod's node into it, so the controller reads
the node from the token instead of believing the request (ADR 0040). A token that
was not projected into a running pod carries no node and is refused. Resolving
the OIDC discovery and JWKS endpoints is a read-only non-resource URL `GET`.

### Cluster-wide vs. namespace-scoped installation

- **Cluster-wide (default):** a single ClusterRole with the rules above.
- **Namespace-scoped [planned]:** Role + RoleBinding only in namespaces you
  list; no ClusterRole is created. This would be a hard boundary enforced by
  the API server, not by agent configuration. Not built: the agent's informers
  are cluster-wide today, so this mode has no implementation behind it, and the
  trade-offs below describe what it would cost rather than what it costs.

  Honest trade-off: `nodes` and `namespaces` are cluster-scoped resources, so
  in this mode the agent cannot compute node idle capacity, cannot read
  instance types for pricing, and cannot reconcile totals against your cloud
  invoice. It reports requests-vs-usage findings for the permitted namespaces
  only. `priorityclasses` and `storageclasses` are cluster-scoped for the same
  reason, so in this mode `cluster_policy` carries namespace policy without the
  two catalogs: a workload's priority class and a claim's storage class are
  reported by name, and what those names mean is not.

---

## 5. Network access

| Access | Direction | Why | Notes |
|---|---|---|---|
| Pod pprof endpoints **[planned]** | controller → pods | Pull `/debug/pprof/profile` and `/debug/pprof/heap` from Go services that already expose them | Only pods that pass the workload filters are probed; ports taken from `containerPorts`, never blind scans. `pprof`/`ebpf` profiles only. Not built: the agent opens no connection to a pod today |
| kubelet stats, proxied | controller → API server | Poll `/stats/summary` and `/metrics/cadvisor` on every kubelet for usage counters (ADR 0006) | Goes through the API server proxy (`nodes/proxy`, §4) — the agent opens **no direct connection to kubelets**. A direct-kubelet transport for very large clusters would be a documented change here, not a silent one |
| Backend egress **[planned]** | controller → one fixed domain | Ship aggregated rollups and filtered profiles | mTLS, pinned domain. The only cross-boundary connection in the system, and it does not exist yet: the controller writes payloads to its local spool and ships nothing. A NetworkPolicy restricting controller egress to this domain plus in-cluster targets ships with the chart **[planned]** — the chart exists ([`charts/runtime-agent`](../charts/runtime-agent)), the policy does not |
| Node → controller reports | node → controller, in-cluster only | Deliver on-node Go build-info findings and captured profiles for aggregation (ADR 0010, ADR 0011), and ask which pods on this node passed your filters (ADR 0015) | Plain HTTP on the cluster network; the node always initiates and authenticates with a projected controller-audience ServiceAccount token, validated locally (no `TokenReview`). The report endpoints answer ack/error only. The **scope** query answers from the controller's already-filtered pod index, so it can only narrow what the node scans — it cannot name a pod your filters exclude. The **profiling-target** query is the one reply the node acts on; see [§7.2](#72-the-ebpf-cpu-profiler-opt-in-ebpf-profile-adr-0011) for what bounds it. The receiver is not reachable from outside the cluster, and a NetworkPolicy shipped with the chart restricts it to the node DaemonSet (ADR 0040 — it was described as shipping for months before it did, ADR 0039). Read that policy for what it is: it is enforced by your CNI, so on a cluster whose CNI does not implement NetworkPolicy it is inert, and it restricts who may be *heard*, never what a caller may *say*. The DaemonSet has **no** external egress. Honest disclosure: the token and the (already-filtered) facts travel in cleartext in-cluster, so a party who can sniff pod traffic can replay a node's token until it expires. What that buys is now bounded to **the node it was taken from**: the token names the node its bearer runs on, every endpoint refuses a request that speaks for a different one, and a fact can only join to a pod on the reporting node (ADR 0040). So a replayed or compromised node token can post fabricated inventory facts and profiles about workloads on *that* node, and read which of *that* node's pods passed your filters. It cannot read or write anything about any other node, it cannot reach the API, and it controls nothing |

---

## 6. Agent identity and credential lifecycle **[planned]**

**None of this section is built.** The agent holds no backend credential today
and opens no connection outside the cluster. What follows is the designed model;
the reasoning is in [ADR 0005](adr/0005-two-tier-identity.md) and
[ADR 0008](adr/0008-identity-in-secret.md).

- **Two tiers, so no long-lived secret is shared across clusters.** An org API
  key lives in your automation and never enters a cluster; it issues short-TTL
  per-cluster enrollment tokens, and a token is exchanged once for a client
  certificate. A leaked token exposes one cluster for its TTL; a leaked org key is
  revoked in one action without touching a running agent.
- **The private key is generated in-cluster and never leaves it.** The controller
  generates a key pair and submits a CSR authenticated by the enrollment token,
  including a cluster fingerprint so a token applied to the wrong cluster is
  detected rather than silently mixing data. Enrollment is outbound and creates no
  control channel.
- **The key lives in a pre-created, name-scoped Secret** — the product's one write
  grant ([§4](#4-kubernetes-api-access-rbac)) — so identity survives rescheduling
  without a volume. Honest consequence: the key is then in etcd and in any backup
  containing Secrets. Whoever reads it can impersonate this cluster's telemetry
  stream — data poisoning of one cluster, nothing more, since the protocol is
  one-way — recovered by revoke plus re-enroll.
- **Certificates renew over the existing mTLS session** and the backend can revoke
  any cluster's at any time. With no valid credential the agent degrades to
  local-only: it keeps collecting and spooling, ships nothing, logs the condition,
  and neither crash-loops nor retry-storms.
- **The backend's private CA is pinned**, not the system trust store. A
  TLS-intercepting proxy therefore cannot impersonate the backend and cannot pass
  the session through either: with egress interception the backend domain needs a
  bypass rule. This is an install prerequisite, not a detail.

### Storage

The controller keeps a small local volume — an `emptyDir`, always. The agent asks
for no PersistentVolume and offers no setting that would give it one
([ADR 0007](adr/0007-optional-durability.md),
[ADR 0026](adr/0026-no-persistent-volume.md)), so installing it requires no
StorageClass and no provisioning rights. The volume holds one thing: the spool of
unshipped payload batches, held until delivery is acknowledged and deleted
immediately after. Only data that has already passed the filters is written
there.

**It cannot fill your node** (ADR 0042). Three bounds, in order: payloads older
than 24 hours are deleted even unacknowledged; the spool holds at most 512 MiB
and 20000 files, dropping the oldest first; and the `emptyDir` declares a 1 GiB
`sizeLimit`, deliberately above the agent's own budget so the agent's bound is
the one that acts and the kubelet's is what holds if it fails. This is stated
because the earlier version did not work: the sweep ran only when a usage record
was written, so on a cluster where no kubelet could be polled nothing was ever
swept.

Everything else the collector keeps — counter baselines, open windows, profiling
rotation state — is held **in memory only**. Nothing about your cluster is
accounted for on disk outside the spool.

- The volume is for continuity, not truth: everything on it is reconstructible,
  so it needs no backups.
- **Configuration is never cached on it.** Filters are read from the ConfigMap at
  every start; a stale filter set cannot resurrect from disk.
- Encryption at rest is your node's. Nothing on the volume is a secret in any
  case — it holds only payloads already approved to leave the cluster.
- A rescheduled pod loses the unacknowledged spool, bounded by the flush cadence
  in normal operation and by the outage span if the backend was unreachable, and
  reported as a gap rather than passed over silently (ADR 0007).
- `spool.dir` is a path. Mount durable storage there if you want that span to
  survive a reschedule; the agent behaves identically either way.
---

## 7. Node privileges

The DaemonSet is not installed at all in the `metrics-only` and `pprof` profiles.
It has two functions with different privilege needs (ADR 0009). Whichever runs,
one property holds for both: **the node role has no Kubernetes API access
whatsoever.** Its ServiceAccount is bound to no Role or ClusterRole — not one
RBAC rule exists for it — and `automountServiceAccountToken: false` keeps the
default API token out of the container. It mounts exactly one credential: a
projected token audience-bound to the controller, which the API server rejects
for the wrong audience and which grants nothing anyway. Two independent
barriers, both enforced by the API server rather than by agent configuration.

That credential is also what limits a node to itself. Every pod of the DaemonSet
presents the same ServiceAccount, so the subject says which role is calling and
nothing about which node — but the token is bound to the pod it was projected
into, and the kubelet records that pod's node inside it. The controller reads the
node from there and refuses any request that speaks for a different one, so a
node that is compromised, or whose token is sniffed off the cleartext channel, is
confined to facts and queries about **that node** (ADR 0040).

### 7.1 The Go-binary scanner (the current node role)

This is what the node DaemonSet runs today: it enumerates processes under
`/proc`, reads the Go build information embedded in each executable, ties it to a
pod and container through the process cgroup, filters infrastructure on the node,
and reports the result. It loads no eBPF; the only socket it opens is the
outbound connection to the controller (ADR 0010) — never to the internet, never
to the API server. What it reads is `/proc/<pid>/exe`, `/proc/<pid>/cgroup` and
two fields of `/proc/<pid>/status`, and nothing else.

Those two fields are `VmHWM` — the kernel's high-water mark of the process's
resident memory, which is the only place a memory peak between two samples is
recorded — and `Cpus_allowed_list`, of which only the *count* of permitted CPUs
leaves; which cores a container sits on is not collected
([ADR 0052](adr/0052-the-peak-the-kernel-remembers.md)). The file also opens with
`Name`, the executable's own basename: the fields are an allow-list, so it and
everything else in the file stay on the node. The read needs no capability —
the kernel's ptrace check guards `exe`, `mem`, `environ` and `maps`, not
`status` — and happens only for a process already kept, after the scope and
infrastructure filters.

| Privilege | Why |
|---|---|
| `hostPID: true` | See node processes and open their `/proc/<pid>/exe` to read Go `buildinfo` without entering containers |
| `CAP_SYS_PTRACE` | The **only** added capability. Container processes are non-dumpable, so the kernel's ptrace access check blocks reading another container's `/proc/<pid>/exe` even for a same-UID root reader — verified empirically. It is **not** an eBPF capability |
| host `/proc`, read-only | Mounted at `/host/proc`. No other host path is mounted. With `hostPID` this mount is a convenience rather than a boundary: the container's own `/proc` already shows the same process table, mounted read-write by the runtime |

**What this privilege actually is, stated before the controls that do not bound
it** ([ADR 0039](adr/0039-stated-limits-are-measured-limits.md)). `hostPID` plus
`CAP_SYS_PTRACE` plus uid 0 passes the kernel's ptrace access check against every
process on the node, and `/proc/<pid>/root` then resolves in that process's own
mount namespace. Code running in this container can:

- **read any other container's filesystem on that node**, including its
  ServiceAccount token and every Secret volume mounted into it — so the node role
  does reach other workloads' secrets, by a path that has nothing to do with the
  API server and is therefore not excluded by
  [§9](#9-what-the-agent-does-not-request)'s "no `secrets`" row;
- **read any host process's memory** (`/proc/<pid>/mem`, `PTRACE_ATTACH`),
  recovering keys that were never written to disk;
- **write** to that memory and to the host filesystem through `/proc/1/root`,
  which is code execution as host root — the read-only flag on `/host/proc` does
  not prevent it, for the reason in the table above;
- **signal host processes**, kubelet and the container runtime included.

By default the DaemonSet tolerates every taint, so it schedules on control-plane
nodes too. That matters on a self-managed cluster — kubeadm, kops, bare metal —
where the API server, etcd and the service-account signing key are processes and
files on a node this pod can land on, and the access above reaches all three. On
a managed control plane the control plane is not in your node pool. Change
`node.tolerations` to narrow it, and note the cost: an excluded node contributes
no inventory and no profiles, silently.

**Installing the DaemonSet is therefore a decision to trust this agent's image
with the node**, and on a control-plane node with the cluster. That is the honest
question to weigh — not whether the controls below are present. They are present,
and none of them narrows the paragraphs above: they bound what this agent's own
code does by accident, not what code in that container could do on purpose.

Compensating controls, set by the chart and asserted against its rendered output
by `internal/chartrender`: `privileged: false`,
`allowPrivilegeEscalation: false`, a read-only root filesystem,
`seccompProfile: RuntimeDefault`, and all capabilities dropped except
`SYS_PTRACE` — no `CAP_BPF`, no `CAP_PERFMON`.

It runs as UID 0, the only component in the product that does and the only one
that asks for it explicitly rather than inheriting it
([ADR 0037](adr/0037-root-is-one-roles-exception.md)). Not because root is the
only way past the kernel's check — `CAP_SYS_PTRACE` satisfies that on its own —
but because a capability granted to a non-root process does not survive `execve`
unless it is in the *ambient* set, which Kubernetes does not populate. Measured
on 1.36: two pods differing only in uid, reading `/proc/1/exe` succeeds as uid 0
and is denied as uid 65532.

What genuinely does not follow: the node role still holds **no Kubernetes API
access**, so none of this is a path to reading Secrets through the API server, to
changing an object, or to reaching another node. The blast radius is the node and
what is mounted on it.

### 7.2 The eBPF CPU profiler (opt-in `ebpf` profile, ADR 0011)

Off by default. When enabled it adds, on top of the scanner's privileges, exactly
two capabilities and one read-only host mount — never `privileged`:

| Privilege | Why |
|---|---|
| `CAP_BPF` + `CAP_PERFMON` | Load eBPF programs and perform perf sampling. The *minimum* for the embedded profiler; `privileged` is not used and `allowPrivilegeEscalation` stays `false` |
| host `/sys/kernel`, read-only | Kernel BTF, required by the profiler's CO-RE programs. **The mount is the parent directory, not `/sys/kernel/btf`** — a `hostPath` of a missing directory blocks the pod from starting, and on a kernel without BTF `btf` does not exist, which would defeat the graceful refusal below. So the mount also exposes `security/`, `debug/`, `mm/` and the rest of `/sys/kernel` for reading. A `hostPath`'s read-only flag is not recursive, so a submount such as debugfs is not covered by it |

**Kernel floor with a graceful refusal.** `CAP_BPF`/`CAP_PERFMON` exist only on
kernel 5.8+, and the profiler needs BTF. Lacking either, the profiler does not
start, the unmet requirement is reported and counted, and the Go-binary scanner
keeps running — degraded and visible, never silently escalated to `privileged`.

**Symbolization happens on the node.** Your binaries are never copied or
uploaded; stack addresses are resolved locally against the running binary, and
only symbolized, allow-list-filtered profiles leave the node — towards the
in-cluster controller.

**Sampling is node-wide; the filters apply to what is shipped, not to what is
captured.** Perf sampling attaches to the node rather than to a process set, so
the profiler symbolizes frames from every process on it — pods your filters
exclude, and host processes such as the kubelet — and those frames sit in the
node's in-memory buffer until the window is filtered and shipped. This is the one
collection path where the filter-early principle of
[§1](#1-design-principles) is not met, and it is a property of node-level
profiling rather than a choice made here
([§10.2](#102-node-level-visibility-cannot-be-namespace-scoped), ADR 0039). If
that is not acceptable, the `ebpf` profile is not for you.

**What decides who gets profiled.** Profiling is off unless you deploy the node
DaemonSet with the `ebpf` profile *and* enable it on the controller. Once on, the
workloads it may profile are **the workloads you already chose to collect**;
there is no second namespace list to keep in step with the first (ADR 0025). On
its own interval the node asks the controller which of those rank highest by
consumption, and the reply is a list of container IDs on that node — the one
node↔controller reply the node acts on. It is bounded by construction: the
ranking comes from usage rollups, which exist only for pods your filters
admitted. Two answers must admit a container before its samples are shipped, that
reply and the scan scope of
[§10.2](#102-node-level-visibility-cannot-be-namespace-scoped). A node that
cannot reach the controller profiles nothing.

**Which bound is enforced where, and what each is worth** — a table because the
distinction is the whole security argument, and it was previously stated wrongly
(ADR 0025):

| Bound | Enforced from | Stops a buggy controller | Stops a hostile one |
|---|---|---|---|
| Symbol allow-list — which module prefixes may leave in a frame | the node's own ConfigMap | yes | **yes, with the exemption named below** |
| Overhead ceiling, capture duration, interval | the node's own ConfigMap | yes | **yes** |
| Profile validation (parses, `cpu`/`nanoseconds`, a service function present) | the node's code | yes | **yes** |
| The container exists on this node | the node's `/proc` | yes | **yes** |
| Your collection filters, via the targets reply | the controller | yes | no |
| Scan scope | the controller's reply | yes | no |

The bottom two are facts the node receives rather than facts it holds, so they
bound a controller that is wrong and not one that is lying. **This is not a gap
that can be closed** while the node holds zero Kubernetes API access (ADR 0009):
every namespace fact reaches it through the controller, so no node-side re-check
of a controller-supplied label can constrain the controller. Giving the node a
namespace list to check against would look like a safeguard and be none.

The top five are genuinely unforgeable, and the symbol allow-list is the
load-bearing one: it is the reason a stack trace is defensible to ship at all,
and it is enforced on the node from a file your Helm release owns.

**What the allow-list bounds is dependencies, not your own code** (ADR 0041).
Your `main` package is kept without consulting it, and so is any module whose
path has no dot in its first segment — `module acmecorp` rather than
`module github.com/acme/…`. A public Go module path must begin with a domain or
it could not be fetched, so "no domain" means the standard library or code you
did not publish, and you installed the profiler to see the latter. The
alternative was to make you list your own module paths before your own hot
functions appeared, which is the second list ADR 0025 removed. This does not
widen what a hostile controller could extract: targets come from the controller's
already-filtered pod index, so keeping `main` exposes the function names of
workloads your filters admitted and nothing else.

**Source paths do not leave the node.** A frame carries the file's *base name*
only — `server.go`, never a directory — taken where the profiler hands the frame
over, so the full path is never held anywhere in the agent. A Go binary built
without `-trimpath`, the default, records the absolute path of the machine that
compiled it: without this, a profile would carry your CI workspace, your internal
VCS hostname and the layout of your source tree. Line numbers are kept.
---

## 8. Data collected and data leaving the cluster

This is usually the most important section for review. Data falls into four
classes with different sensitivity and different default policies:

| Data class | Examples | Sensitivity | Default policy |
|---|---|---|---|
| Resource metrics | CPU usage, working set, throttling ratios, requests/limits, node allocatable | Low (numbers) | Collected for everything visible, minus the infrastructure deny-list and your filters |
| Workload metadata | Workload names, namespaces, image digests, Go version, module paths and versions, node zone and instance type | Medium | Same as metrics |
| Object history (journal) | Pod names, container names, restart counts, termination reasons and exit codes | Medium | Same as metrics. This is the one class that names individual **pods** — see below |
| Profiles (stack traces) | Function names, call graphs | **High** — function names reveal the structure of your code | **Explicit allow-list only.** A stack trace never leaves the cluster unless its module path is on the allow-list you configured |

### Every payload the agent produces

This is the complete list — there is no other channel and no other shape, and a
test fails if a kind ships without appearing here (ADR 0022).

| Payload | What it carries | Names pods? |
|---|---|---|
| `collection_coverage` | What the agent did rather than what it found: how many pods and Jobs it observed, how many each of your four controls excluded, how many placement terms the reduction dropped, what the node scanners walked and skipped, which of the agent's reads worked, its version, and the shape of your configuration. **Aggregate counts only — no name of anything you excluded appears here or anywhere else** ([ADR 0054](adr/0054-coverage-says-how-much-was-hidden-never-what.md)) | no |
| `usage_snapshot` | The still-open hour's resource rollup: per (namespace, workload, container) histograms of CPU and memory, throttling and PSI counters, plus how much of the window was observed. Shipped once a minute, each replacing the last | no |
| `usage_window` | The same shape, final, when the hour closes | no |
| `network_window` | Per collected workload and closed hour: bytes and errors received and transmitted, summed over its pods and their interfaces. Interface **names are not read** — only how many there were. Counters are the *pod's*, so nothing here is attributed to a container, and a `host_network` workload's counters are the **node's** and describe the whole machine ([ADR 0053](adr/0053-network-counters-are-the-pods.md)). The kubelet counts bytes at the interface and never says where they went: there is no destination, no peer and no address in this payload | no |
| `oom_kill` | One out-of-memory kill: namespace, pod, container, workload, when it finished, exit code, restart count, and the memory limit in force | **yes** |
| `container_restarts` | Per (namespace, pod, container) and hour: restart count, breakdown by termination reason, how many restarts had no reason visible, last exit code | **yes** |
| `restart_counters` | Per flush: for each collected container that has ever restarted, the kubelet's restart counter as the agent last observed it, how much of it the agent has watched and since when, when the pod was created, how long the current incarnation has been running, and the reason, exit code and instant of the most recent termination. The counter and the watched-since figure are read from one observation, so their difference is never negative (ADR 0043) | **yes** |
| `pod_disruptions` | Per hour: the pods the *cluster* removed — preempted, evicted under node pressure, drained, or removed via the eviction API — with the node and the instant | **yes** |
| `job_runs` | Per hour: each Job that finished — when it started and finished, whether it succeeded, the failure reason, its pod success/failure counts, and its declared `parallelism`/`completions`/`backoffLimit` | **yes** |
| `workload_revisions` | Per flush: each ReplicaSet of a collected workload that manages ReplicaSets — a Deployment, or a custom resource such as an Argo Rollout ([ADR 0049](adr/0049-revisions-are-not-only-deployments.md)) — with when it was created, its desired/current/ready replica counts, each container's image reference, and a revision number for a Deployment only | **yes** |
| `workload_metadata` | Declared shape per (namespace, workload, container, image digest): image, requests, limits, ports, probe schedules without what any probe checks, the five runtime knobs of the field-minimization list below, QoS, replica counts by phase, by node, and by unscheduled reason, the pod's reduced placement constraints (ADR 0031), and — for a Deployment, StatefulSet or DaemonSet — the update strategy by which the workload replaces its own replicas ([ADR 0048](adr/0048-findings-name-the-fields-they-need.md)). A workload of any other kind carries no strategy and nothing standing in for one | no |
| `workload_policy` | Per collected workload, what bounds it from outside its own spec: disruption budgets covering it, autoscalers driving it, the volume claims its pods mount (ADR 0032), and the Services routing to it (ADR 0048) with, per Service, whether topology-aware routing was asked for and how many of its ready endpoints the cluster actually gave a zone hint to ([ADR 0051](adr/0051-topology-routing-is-asked-and-not-granted.md)); plus `unavailable_sources`, naming any of those the agent could not read — never granted, or no longer being fed (ADR 0033, ADR 0035) | not directly — but a claim is named, and a StatefulSet's claim is `<template>-<set>-<ordinal>`, so `data-db-0` identifies pod `db-0` (ADR 0039) |
| `cluster_policy` | Per collected namespace, its LimitRanges and ResourceQuotas; plus the cluster's PriorityClasses and StorageClasses, which workloads reference by name (ADR 0032), and `unavailable_sources` for any of those the agent could not read — never granted, or no longer being fed (ADR 0033, ADR 0035). StorageClass `parameters` are never read | no |
| `node_metadata` | Per node: name, size, instance and capacity type, zone, region, kernel version, CPU architecture. The **name** is your cluster's, and on EKS and on GKE's legacy naming it encodes the node's private address — `ip-10-42-13-201.eu-west-1.compute.internal`. The agent reads no address field; the name it does read may be one (ADR 0039) | no |
| `go_inventory` | Per (namespace, workload, container): Go version, main module, image digest, PGO flag, plus a fleet-coverage block | no |
| `process_peaks` | Per collected workload container and build: the largest `VmHWM` among its Go processes, how many processes that was taken over, and the range of permitted CPU counts. A **floor** under the container's peak, never the figure the OOM killer compares against — the cgroup also holds page cache and every other process in it ([ADR 0052](adr/0052-the-peak-the-kernel-remembers.md)) | no |
| `go_build` | Per image digest, written once: Go version, main module, each dependency module's **path and version**, allow-listed build settings, and two allow-listed GODEBUG defaults. This is a bill of materials for the build — see [§10.4](#104-the-build-inventory-is-a-bill-of-materials) | no |
| `ebpf_profile` | One capture: allow-list-filtered symbolized pprof bytes, keyed by workload, image digest and capture window | no |

Each declares its provenance in a `source` field — `structural` (read from a
spec), `measured` (polled from the kubelet), `journal` (from object history) or
`sampled` (a profiler's estimate)
([ADR 0012](adr/0012-payload-registry-and-provenance.md)).

### Field minimization

- The agent **never reads Secrets or ConfigMaps through the API** — no RBAC for
  either exists — and no payload carries a field derived from one. Its own
  configuration arrives as a mounted file, not through the API. For the two
  non-API paths by which secret material is nonetheless reachable, see
  [§9](#9-what-the-agent-does-not-request).
- **The image reference ships verbatim, and it is the one free-form string here
  with no length bound and no allow-list** (ADR 0039). Know what your registry
  path encodes: a cloud account identifier, a region, your repository taxonomy,
  and whatever your CI writes into a tag.
- From pod specs: `metadata`, `spec.containers[].resources`, `[].ports`, the
  probe schedules and the nine placement fields below, `ownerReferences`, and
  `status`. `envFrom`, `args` and `command` are removed as each object arrives,
  before it enters the agent's cache, so they are not held in memory either, not
  merely kept out of payloads
  ([ADR 0046](adr/0046-the-cache-is-the-source.md)). The
  `kubectl.kubernetes.io/last-applied-configuration` annotation goes with them —
  a verbatim copy of the object as applied, environment included.
- **A probe's schedule is kept; what it checks is not**
  ([ADR 0048](adr/0048-findings-name-the-fields-they-need.md)). Each container's
  liveness, readiness and startup probe ships as its delays, period, timeout and
  thresholds, plus the *kind* of check — `exec`, `httpGet`, `tcpSocket`, `grpc`.
  The handler's contents are cleared as the object arrives, before it enters the
  agent's cache: the exec command, the HTTP path, host and headers (where an
  `Authorization` value is written by hand), the TCP host, the gRPC service. The
  `lifecycle` hooks are removed whole.
- **Five environment variables are kept by name, and no other one is**
  ([ADR 0047](adr/0047-runtime-knobs-are-named-and-kept.md)): `GOMAXPROCS`,
  `GOGC`, `GOMEMLIMIT`, `GODEBUG`, `GOTRACEBACK`. They ship in
  `workload_metadata` as `runtime_env`. The list is in code, not in your values
  file. One of them whose value comes from a `secretKeyRef` or `configMapKeyRef`
  is **not** read; one read from the container's own limits ships as the field
  path (`resource:limits.cpu`), never as a resolved value.
- From `status`: the image digest, the
  restart counter, a terminated state's reason and exit code, the reason and
  transition time of exactly two conditions — `PodScheduled` and
  `DisruptionTarget` — and `status.reason` when it says `Evicted`. Nothing else.
- **Messages are never read**, neither a termination's nor a condition's. A
  reason comes from Kubernetes' own vocabulary; a message is free text and
  routinely names nodes, taints and other teams' workloads. One qualification
  (ADR 0039): tolerations *are* carried verbatim, so if a collected pod tolerates
  `dedicated=payments-gpu`, that key and value ship in its placement block — your
  taint vocabulary leaves with them.
- **Placement constraints.** Nine pod-spec fields say where a pod may run and
  what it costs to move it: `nodeSelector`, `affinity` (node, pod and pod-anti),
  `topologySpreadConstraints`, `tolerations`, `priorityClassName`,
  `terminationGracePeriodSeconds`, `hostNetwork` and `schedulerName`. They are
  **reduced, not copied**
  ([ADR 0031](adr/0031-placement-constraints-are-reduced.md)): affinity keeps
  keys, operators, values, topology keys and whether a term is required, and the
  label selectors inside pod-affinity and spread terms are dropped. Values are
  bounded at 128 bytes and lists at fixed counts, and anything past a bound is
  dropped whole rather than truncated. What was dropped is counted and never
  named. From `volumes` only `persistentVolumeClaim` names are read
  ([ADR 0032](adr/0032-cluster-policy.md)); a Secret, ConfigMap or projected
  volume is skipped without its name being touched.
- From ReplicaSets: `metadata`, the replica counts, and from `spec.template`
  exactly one field per container — the image reference
  ([ADR 0030](adr/0030-deployment-revisions.md)).
- From Jobs: `metadata`, the terminal condition's type, reason and transition
  time, the start and completion times, the succeeded and failed counts, and
  `spec.parallelism`/`completions`/`backoffLimit`. From `spec.template`,
  **annotations only**, to honor an opt-out written there.
- From node objects: size (`allocatable`/`capacity`), the instance-type and
  capacity-type labels, the topology labels, and two fields of `status.nodeInfo` —
  `kernelVersion` and `architecture`. No other labels, annotations, addresses or
  `status` fields, with the caveat in the `node_metadata` row above: on several
  managed platforms the node's *name* is derived from its private address.

### Workload and node metadata (ADR 0012)

Both are snapshots of current state that replace their predecessor rather than
accumulating history, and neither names a pod: replicas are counted, not listed.
A multi-container pod appears as one `workload_metadata` record per container,
each repeating the same pod-scoped and workload-scoped blocks
([ADR 0014](adr/0014-scoped-facts-are-nested.md)). The pod block also counts the
replicas *not* on a node, by the reason the scheduler gave (ADR 0021).

Both are limited to what passes your filters — an excluded pod is absent
entirely, and its identity appears in neither payload — and both carry
`captured_at`, the instant the snapshot was assembled, so a stale snapshot can be
recognized rather than assumed current.

### Object history, and the one place pods are named (ADR 0020)

`workload_revisions` names ReplicaSets, which your cluster created and named.
Any workload that manages ReplicaSets — a Deployment, or a custom resource such
as an Argo Rollout — and only while it has collected pods, read from the same
admitted index everything else uses ([ADR 0030](adr/0030-deployment-revisions.md),
[ADR 0049](adr/0049-revisions-are-not-only-deployments.md)). StatefulSet and
DaemonSet revisions live in `controllerrevisions`, which the agent does not read. `job_runs` names the Job
object, a generated per-run name such as `rollup-29123456`, and the CronJob that
scheduled it; a run whose Job, CronJob, namespace or pod template carries the
opt-out annotation produces no record ([ADR 0029](adr/0029-job-runs.md)).

`oom_kill`, `container_restarts`, `restart_counters` and `pod_disruptions` report
what object status records about a container's history. All four are `journal`
provenance and all four name the **pod**. `pod_disruptions` is what the *cluster*
did to a pod — preempted, evicted, drained, removed via the eviction API; a pod
that failed on its own belongs to the restart journal.

`restart_counters` is the counter itself rather than a record of restarts: the
kubelet's running total, including restarts from before the agent was installed,
with how many of them the agent watched
([ADR 0034](adr/0034-restart-counter-readings.md)). It is not a new read — the
counter arrives in the pod status the agent has always watched.

**Why the pod is named here and nowhere else.** A crash loop is not actionable
without knowing which replica, and the pod name is one your cluster generated,
never workload content. An excluded pod produces no journal record, and the
exclusion also removes what was already collected
([§11](#11-your-controls-and-what-the-agent-says-about-itself)).

Reason breakdowns use a fixed set (`OOMKilled`, `Error`, `Completed`,
`ContainerCannotRun`, `ContainerStatusUnknown`, `DeadlineExceeded`, `StartError`,
`Evicted`); anything else is counted under `other`, so the payload's shape stays
bounded. Where a reason is a single *value* rather than a field name it is
carried as your kubelet wrote it. Messages are never read.

### What the agent says about its own collection (ADR 0013)

Usage payloads carry what the agent observed while producing them — metadata
about its own polling, describing no workload and identifying nothing in your
cluster: an `observation` block per payload (poll cadence, cumulative kubelet
requests attempted and failed, which kubelet signals this cluster exposes), and
per record, how much of the window that container was observed for.

Without it, "never throttled" and "never scraped" produce identical records, and
the distinction is unrecoverable once the moment has passed.

The same honesty applies to the API server. A cache the agent cannot read is
declared in `unavailable_sources` on the payload that reads it — whether never
granted or taken away from the running agent — and a cache the agent cannot work
without stops the agent instead
([ADR 0035](adr/0035-watch-failures-are-noticed.md)). Both name a resource class
and nothing else; the underlying error stays in the agent's log.

### Profile filtering (applies on the node, before egress — ADR 0011)

- Stack frames are filtered by Go module path on the node that captured them.
  **Your own code is kept** — the standard library, your `main`, and any module
  declared without a domain in its path, since a public module path must begin
  with one. Frames on **your allow-list** are kept. **Third-party dependency
  frames are dropped by default.** Everything else — Kubernetes components,
  service meshes, observability stacks, this agent itself — is dropped. Only kept
  frames and an aggregate drop count leave the node, never the identities of
  dropped functions.
- The allow-list therefore bounds **dependencies**, not your own identifiers
  (ADR 0041). What you exclude from collection you exclude with namespace filters
  and opt-out annotations; profiling scope follows collection scope (ADR 0025).
- A kept frame carries its source file's **base name and line number** —
  `server.go`, never a directory — cut where the profiler hands the frame over,
  so the path the compiler recorded never enters the agent (ADR 0041).
- Profiles are **validated on the node before they are queued to leave**: shipped
  only if the profile parses, carries a `cpu`/`nanoseconds` sample type, and has
  a non-`runtime.*` function in its top. A failure is dropped and counted. What
  leaves is pprof bytes keyed by (namespace, workload, container, image digest,
  window), declaring `source: sampled` (ADR 0023).
- These are **flame-graph profiles, not PGO profiles**: the output omits what
  Go's PGO requires (`Function.start_line`, inline frames).
- Raw, unfiltered samples exist **in the node process's memory only**, for the
  span of one capture window, and are never written to disk — the node DaemonSet
  mounts no writable path of any kind. Filtered profiles sit in the controller's
  spool until delivery is acknowledged and are deleted right after.
- Aggregated metric rollups are hourly mergeable histograms per workload; no raw
  per-second samples are shipped.

### On-node binary scanning (node role, ADR 0009)

The node role reads Go build information from executables on the node and applies
the filter-early rule there, before any record is formed.

- **Scanned at all — only pods that passed your filters** (ADR 0015). A process's
  cgroup is resolved to its pod first, and a pod outside the scope the controller
  supplied is skipped with its executable unopened. A node with no scope scans
  nothing ([§10.2](#102-node-level-visibility-cannot-be-namespace-scoped)).
- **Kept — customer workload binaries.** Go version, main module path, each
  dependency's module path and recorded version, an allow-listed subset of build
  settings, and the pod UID and container ID from the cgroup. Never source, never
  environment, never arguments.
- **Dropped — infrastructure.** A main module on the built-in deny-list
  (`k8s.io/`, `sigs.k8s.io/`, the container runtime, the CNI, `go.etcd.io/`,
  coredns, prometheus, grafana, … and this agent's own module) is dropped on the
  node, its identity never recorded, only counted.
- **Counted, never identified.** Five aggregate counters cover everything not
  kept: processes scanned, Go binaries found, out of scope, infrastructure,
  unreadable. No identity of a filtered process leaves as anything but a number.

**Build settings are an allow-list, in full**
([ADR 0019](adr/0019-build-settings-by-allow-list.md)):

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

Two GODEBUG defaults are read out of the toolchain's compound `DefaultGODEBUG`
setting and shipped as `godebug`
([ADR 0050](adr/0050-godebug-defaults-are-a-build-fact.md)): `containermaxprocs`
and `updatemaxprocs`, each `0` or `1`, which say whether the binary's runtime
sizes `GOMAXPROCS` from the container's CPU quota. The compound value is parsed,
never shipped whole: the rest of what it carries is TLS, HTTP and type-checker
switches, and none of it is collected.

Everything outside it is discarded on the node, including **`-ldflags`,
`-gcflags`, `-asmflags` and `-tags`** — free-form flags that routinely carry
build-machine paths, internal hostnames and injected version strings — and the
**value of `-pgo`**, a path on the build machine; whether PGO was applied
survives as a boolean. A value over 128 characters is dropped whole rather than
truncated.

The controller joins the kept facts into `go_inventory` and `go_build`
(ADR 0010). A module a `replace` directive redirected ships under the path and
version the build *required*, flagged `replaced`; the replacement's own path is
never read, because a local one is a directory on the build machine. A record
exists only while its workload does, so a deleted or opted-out workload leaves
the payload rather than lingering
([ADR 0018](adr/0018-inventory-records-live-only-while-their-workload-does.md)).
A fact that cannot be resolved is counted rather than guessed, and its pod UID,
container ID and module path never appear. The `coverage` block is cluster-wide
counts that name nothing.

### What the agent reports about your filters

Full information about what is collected, only aggregate information about what
is not. Names of excluded objects are never transmitted — not namespaces, not
workloads, not deny-listed module paths; the only filter content that leaves is
the profile allow-list, which the admitted profiles already reveal. What does
leave is counts: pods observed and excluded per filter, the inventory's
`coverage` block, the usage payloads' `observation` block. Each is a number and
names nothing.

Node-level totals (allocatable, aggregate node usage) are collected regardless of
workload filters. They attribute to no workload and are what reconciles cluster
cost against your invoice.

The per-filter exclusion counts leave in the `collection_coverage` payload,
beside the *shape* of the configuration in force — how many entries each filter
list holds and when the file last changed, never a name from it. There is
deliberately **no hash of your configuration**: a digest of a few short
namespace names is reversible by trying the plausible ones, and what it would
expose is your deny list ([ADR 0054](adr/0054-coverage-says-how-much-was-hidden-never-what.md)).

### Transport **[planned]**

Nothing is transmitted today: payloads are written to the local spool and deleted
by its bounds. The designed transport is controller → backend over mTLS to a
single fixed domain, carrying the payload kinds above and nothing else.

---

## 9. What the agent does NOT request

Stated explicitly so it does not have to be asked. This section lists what is
**not requested**; two rows used to be read as listing what is *not reachable*,
which is a stronger claim and was not true (ADR 0039).

- No `secrets`, no `configmaps` — no RBAC for either exists anywhere, so the
  controller cannot read one through the API server, and the API server enforces
  that rather than agent configuration. The ClusterRole's resource list is closed
  rather than merely reviewed: `internal/chartrender/guardrail_test.go` names
  every resource the chart may request and fails on anything else, in every
  profile. This is a statement about the API path only: secret *contents* remain
  reachable by the controller's identity through `nodes/proxy`, and on a node by
  the DaemonSet's `/proc` access
  ([§7.1](#71-the-go-binary-scanner-the-current-node-role)). What keeps them out
  of payloads is the collection code — pod `env`, `args` and `command` are fields
  of no collected struct — asserted by tests rather than by the API server.
- No `pods/exec`, `pods/attach`, `pods/portforward` **as named resources**. Not
  the same as "cannot exec": `get` on `nodes/proxy` reaches the kubelet's own
  `exec`, `attach` and `portForward` routes, which it serves over GET
  ([§4](#4-kubernetes-api-access-rbac)). The agent calls two stats paths and
  nothing else, and that bound lives in the code.
- **No write verb except one**, and it is named: `get`/`update` on the agent's
  own identity Secret, scoped by `resourceNames` ([§4](#4-kubernetes-api-access-rbac),
  and **[planned]**). Nothing else carries `create`, `update`, `patch`, `delete`
  or `deletecollection`. That is a true statement about verbs; for what it does
  not imply, see the row above.
- No cloud provider credentials, IAM roles, or billing API access.
- No external egress from nodes — only the controller crosses the boundary.
- No access to your monitoring stack: not Prometheus, Thanos, Mimir, or any other
  metrics store, neither as an option nor to backfill history at install time
  ([§10.1](#101-the-agent-knows-nothing-about-the-time-before-it-was-installed)).
- No API access from the node role at all ([§7](#7-node-privileges)) — zero RBAC,
  default token not mounted.
- No dynamic configuration from the backend — the agent cannot be reconfigured
  remotely.

## 10. Known limitations and honest disclosures

### 10.1 The agent knows nothing about the time before it was installed

Every measurement the agent reports it made itself, from the kubelet counters of
ADR 0006. It does **not** read your Prometheus — not as an option, not for a
one-time backfill — so a cluster installed today has no usage data for yesterday,
and a finding that needs a long observation window has to wait for it.

This is a deliberate refusal about *measurability*: a Prometheus series carries
no record of the recording rules, downsampling and relabelling that shaped it, so
it could declare neither its provenance nor its completeness while being joined,
under the same keys, with data that declares both. Its tenancy is also weaker
than every other path here — the endpoint typically serves all namespaces
regardless of the caller's permissions
([ADR 0016](adr/0016-no-prometheus-as-a-source.md)).

What does not need history still works on day one: declared requests and limits,
QoS, topology and placement, and the object journal are all true at install time.
What genuinely needs observation reports how long it has been observing.

### 10.2 Node-level visibility cannot be namespace-scoped

With `hostPID` and eBPF the node component technically observes all processes on
a node, including pods outside your filters — a property of node-level profiling,
not of this agent. If that is not acceptable, use the `pprof` or `metrics-only`
profile, where no node component exists.

The two node functions differ, and the difference is worth being precise about
(ADR 0039). The **scanner** avoids the exposure rather than mitigating it: it
reads a process's cgroup first and never opens the executable of a pod outside
your scope, so nothing is extracted to discard. The **profiler** cannot: perf
sampling attaches to the node, so frames from every process — excluded
namespaces, and host processes such as the kubelet — are symbolized into the
node's memory and dropped afterwards. Same outcome at the cluster boundary,
different guarantee inside it, and only the first is "not collected".

**How the node knows your namespaces**
([ADR 0015](adr/0015-node-scan-scope.md)). The node holds no API access, and a
cgroup tells it a pod UID and never a namespace, so it cannot evaluate your
filters itself. On each pass it asks the controller which pods on this node
passed them, and scans only those. Two properties follow: a process outside that
set has its executable **never opened**, so no module path is extracted for a pod
you excluded and only an aggregate skip count is reported; and it **fails
closed** — a node that cannot reach the controller scans nothing rather than
everything, costing one pass rather than widening what is collected.

A process belonging to no pod — the kubelet, a systemd unit — is never in scope,
because it is in no namespace and so no namespace filter of yours can permit it.

### 10.3 pprof endpoint probing **[planned]**

Not built — the agent makes no request to a pod today. When it does: locating
pprof endpoints means the controller makes HTTP requests to pods, which network
monitoring may flag as internal scanning. Probing will be restricted to workloads
that pass your filters and to ports declared in `containerPorts`. Coordinate with
your security team before enabling the `pprof` profile.

### 10.4 The build inventory is a bill of materials

Each dependency in `go_build` ships with the version the toolchain recorded, so
the payload is a bill of materials for every Go build in your cluster, and known
vulnerabilities can be derived from it by anyone holding it. This is a deliberate
widening of a narrower earlier promise, argued in
[ADR 0048](adr/0048-findings-name-the-fields-they-need.md) §3. The agent ships no
vulnerability finding of its own: it reports what a build is made of, never what
that implies.

---

## 11. Your controls, and what the agent says about itself

### Your controls

Four, and they are the complete list:

| Control | Where | What it does |
|---|---|---|
| **Namespace allow / deny lists** | Helm values → ConfigMap (`filters.namespaces`) | Names, `*` wildcards permitted. An empty allow list admits every namespace; a non-empty one admits only matches. Deny applies on top and wins on conflict |
| **Namespace opt-out annotation** | on the Namespace object | `rebuildstack.co/collect: "false"` excludes every pod in it |
| **Workload opt-out annotation** | on the Deployment, StatefulSet, DaemonSet, CronJob, or a bare Job or ReplicaSet | the same annotation excludes every pod that workload manages. **On the object itself, never in its pod template** — a template annotation is part of the template hash, so writing one there would roll every replica ([ADR 0028](adr/0028-workload-level-opt-out.md)) |
| **Pod opt-out annotation** | on the Pod object | the same annotation excludes that pod |

**Where the workload control does not reach.** The agent reads the workload kinds
above and no others. A pod managed by a custom resource — an Argo Rollout, a
Knative Revision, an in-house operator — has a controller the agent cannot read,
because that would need RBAC this product does not ask for. Such a pod is
**collected**, not excluded: this agent is opt-out by design, and a controller it
cannot read is not evidence that you opted out. The namespace and pod annotations
still work on those workloads, and the coverage report counts the case in
`workload_unknown_kind`, separately from the transient `workload_not_cached`.
Both are counts; no pod or workload is named.

These four scope **everything**, profiling included: the workloads that may be
profiled are the workloads you collect, and there is no separate list for it
(ADR 0025). What profiling adds is not another filter but two deliberate acts —
deploying the node DaemonSet with the `ebpf` profile, and enabling it on the
controller — plus the symbol allow-list of
[§7.2](#72-the-ebpf-cpu-profiler-opt-in-ebpf-profile-adr-0011).

There is **no label-selector filter.** Earlier revisions of this document listed
one; it was never decided in an ADR and never existed in the configuration
([ADR 0024](adr/0024-security-doc-states-what-adrs-state-why.md) §3).

### What the agent says about itself

The agent tallies pods observed and pods excluded by each filter, and those
counts leave in `collection_coverage`, so filtering behavior is verifiable from
what we receive and not only from what you configured. The same payload reports
which of the agent's own reads worked — measured from its caches, not read back
from the rules, so a grant defeated by a webhook reads as failing rather than as
granted. No startup self-audit via `SelfSubjectRulesReview` is performed: it is a
`create` call, and this agent makes none (ADR 0054 §3). A 403 still degrades with
a log line, never a crash-loop or retry storm.

**What those counts can and cannot tell us.** They say how much was excluded and
under which of your four controls, never which object. One inference remains and
is stated rather than hidden ([ADR 0039](adr/0039-stated-limits-are-measured-limits.md)):
a workload that was collected and then opted out disappears from the data, and
that disappearance is visible whether or not a counter explains it. The counter
explains it; without it the conclusion would be that you deleted the workload.

### Opt-out

Any namespace, workload or pod can exclude itself without touching the agent's
configuration:

```yaml
metadata:
  annotations:
    rebuildstack.co/collect: "false"
```

The exclusion applies at the collection stage, and to what was already
collected. The pod leaves the agent's index immediately, stops being named in the
scan scope the controller gives nodes ([§10.2](#102-node-level-visibility-cannot-be-namespace-scoped)),
and the next snapshot of each payload no longer carries it — including the Go
inventory, which is assembled from facts nodes push and therefore has to be told
to forget rather than simply stop learning (ADR 0018). Nothing about an opted-out
pod survives in a later payload.
---

## 12. Reporting a vulnerability

Contact: **TBD** (security contact will be published before the first external
deployment). Please do not open public issues for suspected vulnerabilities.
