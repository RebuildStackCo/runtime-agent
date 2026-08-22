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
Amends: NNNN, NNNN §5          # optional — decisions this one changes
Amended by: NNNN               # optional — later decisions that changed this one

## Context

What situation forces a decision. Constraints, not opinions.

## Decision

What we chose, stated in one place, unambiguously.

## Consequences

What becomes easier, what becomes harder, what we gave up.
```

**The body is immutable once accepted; the header is maintained.** Context,
Decision and Consequences record what was decided when it was decided and are
never edited. A change of mind produces a new ADR: a whole replacement gives the
old file `Status: Superseded by NNNN`, a partial change declares `Amends:` in the
new file and adds `Amended by:` to the old one.

The distinction is that the two reference lines carry information that did not
exist when the file was written, and that its author could not have written. A
reader who opens an old ADR must be able to see, in that file, that the ground
has moved — which is precisely what nine slices of one-directional references
failed to give them (ADR 0022).

`Status:` is a closed vocabulary: `Accepted` or `Superseded by NNNN`, nothing
else. Partial amendment goes in `Amends:`, never in prose appended to the status.

`docs/adr/adr_test.go` enforces all of it — the graph must be symmetric, point
backwards, name only ADRs that exist, and every ADR must appear in the index
below. Forgetting the mirror line fails `make test`, and the failure prints the
line to paste.

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
- [0014. Facts of a different scope than the record's key are nested, not flattened](0014-scoped-facts-are-nested.md)
- [0015. The node asks the controller which pods it may scan, and fails closed](0015-node-scan-scope.md)
- [0016. Prometheus is not a data source — not as an option, not for backfill](0016-no-prometheus-as-a-source.md)
- [0017. Immutable build facts ship once under the image digest; snapshots say when they were taken](0017-build-facts-keyed-by-digest.md)
- [0018. The Go inventory forgets: a record lives only while its workload does](0018-inventory-records-live-only-while-their-workload-does.md)
- [0019. Build settings ship by allow-list, and the payload that carries them is named for the build](0019-build-settings-by-allow-list.md)
- [0020. The restart journal: windowed aggregates, exact counts, sampled reasons](0020-container-restart-journal.md)
- [0021. Pod lifecycle: the scheduler's reason goes where the shortfall already is, the cluster's removals become a journal](0021-pod-lifecycle-journal.md)
- [0022. The payload registry lives in code, and an amendment cannot stay silent](0022-registry-in-code-and-declared-amendments.md)
- [0023. A profile's key names the build; its provenance names the claim](0023-profile-key-and-provenance.md)
