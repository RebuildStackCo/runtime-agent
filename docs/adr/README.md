# Architecture Decision Records

Significant architectural decisions are recorded here, one file per decision,
numbered in order of acceptance. A decision is "significant" when reversing it
later would be expensive: protocol shape, storage model, security boundaries,
public contracts.

The rule: **ADR first, code second.** A change of this class lands in the same
PR as (or after) the ADR that justifies it — never before.

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
