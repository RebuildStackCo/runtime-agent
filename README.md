# runtime-agent

A Kubernetes agent that collects resource-usage rollups, workload metadata and
allow-list-filtered CPU profiles inside your cluster, reduces them in place, and
ships them — strictly one-way — to the RebuildStack backend for efficiency
analysis. All analysis happens outside the cluster: the agent's intelligence is
data reduction, not judgment.

> **Status: early development.** No releases yet, and two designed pieces are
> not built — the backend transport (payloads are written to a local spool and
> go no further) and the Helm chart (`deploy/` holds raw manifests for the
> end-to-end tests). [`docs/security.md`](docs/security.md) marks every claim
> that is not yet built `[planned]`, so a reader can tell a promise from a
> property.

## Components

- **Controller** — a single replica. It holds every Kubernetes API grant in the
  product, polls the kubelets through the API server, and is the only component
  that will talk to the backend.
- **Node DaemonSet** — optional, and the only privileged part. It reads Go build
  information from on-node executables (`hostPID`, `CAP_SYS_PTRACE`) and, when
  you enable the `ebpf` profile, captures CPU profiles (`CAP_BPF`,
  `CAP_PERFMON`, never `privileged`, kernel 5.8+). It holds **zero** Kubernetes
  RBAC and never mounts the default API token; the one credential it carries is
  audience-bound to the controller, which the API server rejects. It opens no
  connection outside the cluster, and stack frames are filtered against your
  module allow-list *on the node* before anything is sent. See
  [§7 of the security overview](docs/security.md).

## Design principles

- **No control channel.** The agent cannot be commanded from outside the
  cluster. All behavior comes from a ConfigMap you deploy, version and diff —
  nothing a peer returns changes what the agent does.
- **Filter early.** Data you exclude is never collected, not collected and
  dropped.
- **Minimal footprint.** The agent reduces data in-cluster; all analysis
  happens on the backend.

## Supported Kubernetes versions

- **Baseline: Kubernetes 1.33+.** The floor tracks the managed providers:
  a minor version is dropped only after it leaves the standard support
  window of GKE, EKS, and AKS alike — announced, never silent.
- **Full signal set: 1.34+ with cgroup v2 nodes.** Pressure metrics (PSI)
  reach the kubelet Summary API behind the `KubeletPSI` feature gate; on
  older clusters the agent detects what each kubelet exposes at runtime,
  collects what is available, and reports the active signal set instead of
  failing. See [ADR 0006](docs/adr/0006-usage-collection-kubelet-rollups.md).

## Documentation

- [Security overview](docs/security.md) — access, RBAC, node privileges, every
  payload that leaves the cluster, your controls, and the known limitations
- [Backend requirements](docs/backend-requirements.md) — the agent↔backend
  contract
- [Architecture decisions](docs/adr/README.md) — why each of the above is the
  way it is
- [Development process](docs/development.md)

**What the agent actually sends** is `internal/sink/registry.go`: one row per
payload kind with its natural key, delivery discipline and provenance, checked
against the committed golden payloads in both directions. That list is code
rather than prose because a document cannot fail a build
([ADR 0022](docs/adr/0022-registry-in-code-and-declared-amendments.md)).

## License

[Apache 2.0](LICENSE)
