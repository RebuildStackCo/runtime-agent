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

## Documentation

- [Security overview](docs/security.md) — access, RBAC, data classes,
  filtering, identity
- [Backend requirements](docs/backend-requirements.md) — the agent↔backend
  contract
- [Architecture decisions](docs/adr/README.md)
- [Development process](docs/development.md)

## License

[Apache 2.0](LICENSE)
