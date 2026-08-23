# Development plan — job runs and rollout revisions (slice C)

Living plan for the two slices that add `job_runs` and `deployment_revisions`.
Not normative like an ADR: it records *how* we build and *why* each fact is
worth collecting. When a slice ships, tick it and correct this file to match what
actually landed.

- Design authority: [ADR 0028](../adr/0028-workload-level-opt-out.md) (C1a),
  ADR 0029 (C1b) and ADR 0030 (C2), each written with its slice.
- Customer contract touched: [`security.md`](../security.md) §4 (the RBAC
  table's *why* column) and §8 (the payload table).
- Precedents this follows: [ADR 0020](../adr/0020-container-restart-journal.md)
  (windowed journal, reasons not messages, baseline on first observation),
  [ADR 0021](../adr/0021-pod-lifecycle-journal.md) (windows as a delivery
  boundary for events), [ADR 0018](../adr/0018-inventory-records-live-only-while-their-workload-does.md)
  (current-state snapshots; history is the backend's job).

## Why these two facts

**Usage rollups systematically misrepresent short-lived workloads.** A Job that
runs ninety seconds inside an hour-long window reports a tiny average. ADR 0013's
`covered_nanoseconds` says the coverage was low but not *why*, and nothing says
"this was a Job, it ran once, and it finished". Requests for CronJobs are
routinely set from the peak run, so this is where a large share of real
overprovisioning hides — and today it is the one workload class the agent
measures but cannot explain. `job_runs` supplies the denominator.

**A usage change with no attributable cause is not actionable.** "CPU doubled at
14:03" is an observation; "CPU doubled at 14:03, and revision 47 rolled out at
14:01" is a finding. The agent already knows both halves and reports neither
join. `deployment_revisions` supplies the second.

Neither slice adds RBAC, an informer, or an API call: `replicasets` and `jobs`
informers are already running in `PodWatcher` for owner resolution, and
`jobs`/`cronjobs`/`replicasets`/`deployments` are already granted
(`deploy/controller.yaml`). What changes is that objects the agent already
watches stop being read only for their `ownerReferences`.

## Invariants every slice must hold

1. **Filter early.** A Job or ReplicaSet in an excluded namespace, or under an
   opted-out namespace, is never collected — not collected and dropped.
2. **Identities of filtered-out objects never leave** (CLAUDE.md invariant 6):
   only aggregate counts per exclusion reason, and those counts stay **separate
   per object kind**. Folding jobs into `pods_observed` would make the coverage
   report state a falsehood about pods.
3. **Never read `spec.template`'s `env`, `args` or `command`** — not from a Job,
   not from a ReplicaSet. C2 reads exactly one field out of a pod template: the
   container image reference, which `workload_metadata` already reports from
   pods.
4. **Reasons yes, messages no** (ADR 0020 §6). `status.conditions[].reason` is
   Kubernetes' own closed vocabulary; the adjacent `message` is free text that
   routinely names nodes, taints and other workloads.
5. **No new state that must survive** (ADR 0003, ADR 0026). C1's journal is
   in-memory and windowed like the others; C2 derives from live listers and holds
   nothing.
6. **Sink invariant.** New kind means a registry row, a golden, and a row in
   `security.md`'s payload table — `TestEveryShippedKindIsRegistered` and
   `TestSecurityDocPayloadTableMirrorsTheRegistry` fail otherwise.

## Slice ordering

C1a → C1b → C2. C1a first because it changes the customer contract and gates
everything; C1b and C2 are independent of each other.

---

## Slice C1a — the opt-out annotation works on the workload  ✅ MERGED (#56)

Not originally planned. It surfaced while designing C1b's filter: a Job's pods
inherit `spec.template.metadata.annotations`, so the documented way to opt a
CronJob out is a template annotation — and filtering Jobs by the Job object's
own annotations would have collected `job_runs` for a workload whose pods were
excluded. Pulling that thread found the larger defect: a pod-template annotation
is part of the template hash, so the only documented way to exclude a workload
was to roll it.

Locks: the annotation is honored on the controller object itself (Deployment,
StatefulSet, DaemonSet, CronJob, bare Job or ReplicaSet), never on its pod
template; the workload step is attributed before the pod step; it **fails open**,
because the product is opt-out and an unreadable controller is not an opt-out;
and the blind spot is counted in two separate numbers, `workload_unknown_kind`
and `workload_not_cached`. `PodFilter` becomes `Filter`.

---

## Slice C1b — `job_runs`  ✅

**Kind:** `job_runs` · **source:** `journal` · **key:** (window start, window
length) · **delivery:** supersedes

Windowed, exactly like `container_restarts` and `pod_disruptions`, and for ADR
0021's reason: a fleet of CronJobs can complete hundreds of runs an hour, and one
file per run would put the spool's file count under the cluster's control. The
window is a delivery boundary, not an aggregation — each run stands alone and
carries its own instants.

**One record per completed run:**

| Field | Source | Why |
|---|---|---|
| `namespace` | object | Join key |
| `workload` | `ownerReferences` → CronJob, else the Job itself | So many runs of one CronJob aggregate; a bare Job is its own workload |
| `job_name` | `metadata.name` | Which run. For a CronJob this is the generated per-run name |
| `started_at` | `status.startTime` | The denominator the rollup lacks |
| `finished_at` | `status.completionTime`, or the terminal condition's `lastTransitionTime` for failures | `completionTime` is set only on success |
| `result` | `Complete` / `Failed` condition | The outcome, from Kubernetes' vocabulary |
| `failure_reason` | terminal condition's `reason` | `BackoffLimitExceeded` and `DeadlineExceeded` mean different things to a capacity analysis. No `message` (invariant 4) |
| `succeeded`, `failed` | `status.succeeded` / `status.failed` | Pod-level outcome counts: one failure inside a job that eventually succeeded is a retry, and retries cost resources |
| `parallelism`, `completions`, `backoff_limit` | `spec` | The declared shape. Without it "3 succeeded" cannot be read as complete or partial |

**Not carried:** requests and limits. They are `workload_metadata`'s job, and
duplicating them here would give two answers to one question. Consequence to
state in the ADR: when a Job's pods are reaped before the agent sees them, the
run is reported with no resource envelope to join to.

**Reporting rule — corrected while building: no baseline.** The plan said to
baseline already-terminal Jobs on the ADR 0020 §5 precedent. That precedent does
not transfer. Restarts are baselined because a restart counter carries no time;
a Job carries `startTime`, `completionTime` and its condition's transition time,
so an already-finished run files itself in the window where it actually
finished. This is ADR 0021's distinction, and the agent gains the cluster's
recent batch history at startup instead of discarding it. The startup burst is
bounded by the cluster's own retention — three successful and one failed Job per
CronJob by default.

**Honest limitation:** with `ttlSecondsAfterFinished` set, a Job disappears
shortly after finishing, so a run completed while the agent was down is lost.
This is not new — `oom_kill`, `container_restarts` and `pod_disruptions` all
have it — and the coverage report is where it shows.

**Filtering a Job** reuses C1a's chain and adds one step: namespace allow/deny →
namespace annotation → the owning CronJob's annotation → the Job's own → **its
pod template's**. The last step is still needed after C1a, because a customer may
have opted out the old way, through the template. Those pods are excluded, and
`job_runs` must agree with them or the agent ships facts about a workload the
customer refused.

Job counters stay separate from pod counters (invariant 2). C2 needs no
equivalent: it derives from the admitted pod index.

**Deliverables:** ADR 0029 · `internal/journal/jobs.go` + accumulator · watcher
handler on the existing Job informer · `Spool.WriteJobRuns` + payload struct ·
registry row · golden · `security.md` payload-table row and the §4 RBAC *why*
correction · unit tests · this file ticked.

---

## Slice C2 — `deployment_revisions`

**Kind:** `deployment_revisions` · **source:** `structural` · **key:** the kind
itself (one per cluster) · **delivery:** supersedes

A superseding snapshot, like `workload_metadata`, not a journal. The live
revision set is small — `revisionHistoryLimit` defaults to 10 — and every fact is
already on the ReplicaSet object, so a snapshot derived from the lister holds no
state and is loss-harmless by construction. History beyond what the cluster keeps
is accumulated by the backend across snapshots, exactly as ADR 0018 decided for
`go_inventory`.

**One record per ReplicaSet of a collected Deployment:**

| Field | Source | Why |
|---|---|---|
| `namespace`, `workload` | the owning Deployment | Join to usage and metadata |
| `revision` | `deployment.kubernetes.io/revision` | The rollout's identity |
| `created_at` | `metadata.creationTimestamp` | When this revision first existed |
| `replicas` | `spec.replicas`, `status.replicas`, `status.ready_replicas` | Which revision is actually carrying traffic — a rollout in progress has two live at once, and a stuck one has a revision with zero ready |
| `containers[].image` | `spec.template.spec.containers[].image` | What changed between revisions. Exactly this field, nothing else from the template (invariant 3) |

**Scope: Deployments only.** StatefulSet and DaemonSet revisions live in
`controllerrevisions`, which the agent has no RBAC for and will not request for
this. The kind's name says so rather than implying a generality it lacks.

**Two honest limitations for the ADR:**

- `created_at` is when the ReplicaSet was created, not when the revision became
  active. A rollback reuses the old ReplicaSet and gives it a new revision
  number, so its `created_at` still points at the original rollout. The agent
  reports the object's fact and draws no conclusion (ADR 0004).
- `image` is the template's reference, usually a tag, while `go_build` is keyed
  by image digest. The join from revision to build therefore runs through pods,
  which carry the digest — and only works while those pods exist.

**Which Deployments are collected** is inherited, not re-decided: the record set
is built from the workload refs in `PodWatcher`'s admitted index. One admission
decision, one lifetime, no second source of truth — the property
`podIndexEntry.info` already relies on. A Deployment with no admitted pods
produces no record.

**Deliverables:** ADR 0030 · `internal/revisions/` reducer · `Spool.WriteDeploymentRevisions`
+ payload struct · registry row · golden · `security.md` payload-table row ·
unit tests · this file ticked.

---

## Checklist per slice

- [ ] ADR in the same PR, `Amends:` where it changes an earlier decision
- [ ] Registry row + golden + `security.md` payload table (the tests enforce all three)
- [ ] `security.md` updated **iff** the slice changes what is collected/transmitted — both do
- [ ] Filter counters stay per object kind
- [ ] No `env`/`args`/`command` read anywhere
- [ ] `make test` and `make lint` clean; new negative tests actually fail when inverted
