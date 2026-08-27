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
- [0024. `security.md` states what is true; the decision records state why](0024-security-doc-states-what-adrs-state-why.md)
- [0025. The node's configuration holds only what the node can enforce](0025-node-config-holds-only-what-the-node-enforces.md)
- [0026. The controller needs no persistent volume, and therefore no StatefulSet](0026-no-persistent-volume.md)
- [0027. No payload carries an ordering field: the spool holds one version of a key](0027-no-payload-ordering-field.md)
- [0028. The opt-out annotation works on the workload, and the workload step fails open](0028-workload-level-opt-out.md)
- [0029. Finished Job runs ship as a windowed journal, and are not baselined](0029-job-runs.md)
- [0030. Deployment revisions ship as current state, scoped by the pods already admitted](0030-deployment-revisions.md)
- [0031. Placement constraints are collected, reduced rather than copied](0031-placement-constraints-are-reduced.md)
- [0032. What bounds a workload from outside its own spec, and the first widening of read access](0032-cluster-policy.md)
- [0033. A permission the agent was not given degrades one payload, and says so](0033-policy-sources-degrade.md)
- [0034. The restart counter is shipped as a reading, so the day-one history is not lost](0034-restart-counter-readings.md)
- [0035. A cache that stopped being fed is noticed: gating caches stop the agent, the rest degrade their payload](0035-watch-failures-are-noticed.md)
- [0036. The Helm chart is the only installer, and the install profile is the only switch](0036-chart-is-the-only-installer.md)
- [0037. The image runs as uid 65532; root is one role's exception, asked for by name](0037-root-is-one-roles-exception.md)
- [0038. Vulnerabilities are gated on reachability, not on the module graph](0038-reachability-is-the-gate.md)
- [0039. A stated limit is a measured limit, or it is marked](0039-stated-limits-are-measured-limits.md)
- [0040. The channel authenticates a node, not just the node role](0040-the-channel-authenticates-a-node.md)
- [0041. The symbol allow-list bounds dependencies, not the customer's own code; source paths do not ship](0041-the-allow-list-bounds-dependencies-not-your-own-code.md)
- [0042. The spool is bounded, and its bound does not depend on the cluster being healthy](0042-the-spool-is-bounded.md)
- [0043. Both halves of the counter reading come from one observation](0043-the-counter-reading-comes-from-one-observation.md)
- [0044. One fact in one place, and the places have ceilings](0044-prose-has-a-ceiling.md)
- [0045. A kubelet read is bounded in time and in size, and one node's silence costs one node](0045-a-kubelet-read-is-bounded.md)
- [0046. The informer cache is the source, so the fields the agent promises not to collect are removed there](0046-the-cache-is-the-source.md)
