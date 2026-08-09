# runtime-agent

A Kubernetes agent that collects resource-usage data and Go runtime profiles
from workloads in your cluster and ships them — strictly one-way — to the
RebuildStack backend for efficiency analysis.

> **Status: early development.** No releases yet. The design documents below
> are the current source of truth.

## Design principles

- **No control channel.** The agent cannot be commanded from outside the
  cluster. All behavior is defined by Helm values you deploy.
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

- [Security overview](docs/security.md) — access, RBAC, data classes,
  filtering, identity
- [Backend requirements](docs/backend-requirements.md) — the agent↔backend
  contract
- [Architecture decisions](docs/adr/README.md)
- [Development process](docs/development.md)

## License

[Apache 2.0](LICENSE)
