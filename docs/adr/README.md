# Architecture Decision Records

Significant architectural decisions are recorded here, one file per decision,
numbered in order of acceptance. A decision is "significant" when reversing it
later would be expensive: protocol shape, storage model, security boundaries,
public contracts.

The rule: **no decision of this class ships undocumented.** The ADR lands in the
same PR as the code, and may be written either before it (when the decision
needs settling first) or from the finished implementation (when building is what
reveals the right shape). What is never acceptable is a merged change of this
class with no ADR, or an ADR that describes something other than what shipped.

## Format

Files are named `NNNN-short-title.md` and follow a three-section template:

```markdown
# NNNN. Title

Date: YYYY-MM-DD
Status: Accepted | Superseded by NNNN

## Context

What situation forces a decision. Constraints, not opinions.

## Decision

What we chose, stated in one place, unambiguously.

## Consequences

What becomes easier, what becomes harder, what we gave up.
```

ADRs are immutable once accepted. A change of mind produces a new ADR that
supersedes the old one; the old file gets `Status: Superseded by NNNN` and
stays in place.

## Index

- [0001. Agents are never controlled from the server](0001-one-way-protocol.md)
- [0002. Wire format: protobuf over plain HTTPS POST](0002-protobuf-over-https.md)
- [0003. Local storage: spool files and a checkpoint, no embedded database](0003-spool-and-checkpoint-no-database.md)
- [0004. Resource units are the truth; dollars are rendering](0004-resource-units-not-dollars.md)
- [0005. Two-tier credentials with in-cluster key generation](0005-two-tier-identity.md)
- [0006. Usage collection: kubelet counters, mergeable windows, snapshot delivery](0006-usage-collection-kubelet-rollups.md)
- [0007. Durability is optional: snapshot delivery bounds loss, disk is a knob](0007-optional-durability.md)
- [0008. Agent identity lives in a namespaced Secret — the product's one write grant](0008-identity-in-secret.md)
- [0009. The node role: an on-node Go-binary scanner with zero API access](0009-node-role-binary-scanner.md)
- [0010. Node → controller inventory channel: node-initiated HTTP, projected-token auth, local JWKS validation](0010-node-to-controller-inventory-channel.md)
- [0011. On-node eBPF CPU profiling: embedded profiler, node-side symbolization, config-bounded targeting query](0011-node-ebpf-cpu-profiling.md)
- [0012. Payload registry, provenance discriminator, and metadata delivery](0012-payload-registry-and-provenance.md)
- [0013. Observation completeness: measured payloads carry what the agent saw](0013-observation-completeness.md)
