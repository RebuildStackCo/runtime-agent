# 0029. Finished Job runs ship as a windowed journal, and are not baselined

Date: 2026-08-23
Status: Accepted

Adds the payload kind `job_runs`. Nothing existing changes shape; the filter
gains a Job path built on ADR 0028's workload step.

## Context

Usage rollups systematically misrepresent short-lived workloads. A Job that runs
for ninety seconds inside an hour-long window reports a tiny average, because the
average is over the window and the workload existed for a fortieth of it. ADR
0013's `covered_nanoseconds` says the coverage was low; it does not say the
workload was a batch run that started, did its work, and finished.

That gap lands where it costs most. Requests for CronJobs are routinely set from
the peak run and then left, so a batch fleet is where overprovisioning
accumulates quietly — and it is the one workload class the agent measures but
cannot explain. Every fact needed to explain it is already on the Job object,
already watched by an informer the controller runs for owner resolution, and
already covered by RBAC the product has.

## Decision

**1. `job_runs` is a windowed journal, one record per finished run.** Kind
`job_runs`, source `journal`, natural key (window start, window length),
superseding — the same shape as `container_restarts` and `pod_disruptions`.

The window is a delivery boundary, not an aggregation: a run finishes once and
carries its own instants. It exists for ADR 0021's reason, which applies harder
here. A fleet of CronJobs can finish hundreds of runs an hour, and one file per
run would put the spool's file count under the cluster's control.

The record carries the run's namespace, its workload (the scheduling CronJob, or
the Job itself when it has none), the run's own object name, `started_at` and
`finished_at`, the result, the failure reason, the run's pod success and failure
counts, and the declared `parallelism`, `completions` and `backoffLimit`.

The pod counts and the declared shape are there because without them the outcome
is unreadable. A run that succeeded with `failed: 2` retried twice, and those
retries consumed resources nothing else in the protocol reports. "Three
succeeded" is a complete run against `completions: 3` and a third of one against
`completions: 9`.

**2. Failure reasons yes, messages no.** The terminal condition's `reason` is
Kubernetes' own vocabulary, and `BackoffLimitExceeded` and `DeadlineExceeded` are
different facts for a capacity analysis: one is a workload that cannot succeed,
the other one that cannot succeed *in time*, which is often a resource
statement. The adjacent free-text `message` is never read (ADR 0020 §6).

**3. Runs are not baselined on first observation, and this is the opposite of
ADR 0020 §5.** A Job already finished when the informer syncs is reported, and it
lands in the window where it actually finished.

ADR 0020 baselines restart counters because a restart counter carries no time:
reporting a pre-existing count would file history in whichever window happens to
be open at startup, dating events by when the agent started. A Job carries
`status.startTime`, `status.completionTime` and its condition's transition time,
so nothing has to be invented — this is ADR 0021's distinction, made there for
disruptions, and it applies here for the same reason.

The startup burst is bounded by the cluster's own retention: a CronJob keeps
three successful and one failed Job by default, and `ttlSecondsAfterFinished`
removes them sooner where it is set. What the agent gains is the recent history
of a cluster it just started watching, for free and correctly dated.

**4. A run's Job is filtered on four objects, one more than a pod.** Namespace
allow/deny, the namespace's annotation, the owning CronJob's annotation (ADR
0028's workload step), then the Job's own annotations **or its pod template's**.

The last of those is the one that matters. Before ADR 0028 the documented way to
opt a workload out was an annotation in its pod template, so a customer who did
that has excluded pods — and if `job_runs` read only the Job object, the agent
would ship facts about a workload whose pods it refuses to measure. Collected and
sent is worse than collected and dropped, and worst of all when the customer
explicitly asked for neither.

Reading `spec.template.metadata.annotations` is metadata. The `env`, `args` and
`command` beneath it are never read, on a Job no less than on a pod (CLAUDE.md
invariant 4).

**5. Job counters are kept apart from pod counters.** One Job produces many
pods, so a shared `observed` number would answer neither question. The coverage
report gains `jobs_observed` and four `jobs_excluded_*` counters. Aggregates
only; no Job is named (CLAUDE.md invariant 6).

**6. No resource envelope in this payload.** Requests and limits belong to
`workload_metadata`, and repeating them here would give one question two answers
that can disagree.

## Consequences

**Easier.** A batch workload's cost becomes readable: the run took this long,
succeeded on the third try, and was declared with this parallelism, joined to the
usage window it sat in. `covered_nanoseconds` stops being the only hint that
something short-lived happened. And a cluster that starts the agent gets its
recent batch history immediately rather than after an hour.

**Harder / given up.** A run whose Job is reaped before the agent sees it is
lost — `ttlSecondsAfterFinished` set aggressively, and an agent restart across
it. This is the property `oom_kill`, `container_restarts` and `pod_disruptions`
already have, and it is why none of them is a billing record.

A run reported with no resource envelope is possible: if the Job's pods were
reaped before the agent observed them, `workload_metadata` has nothing to join
to. The run's timings and outcome are still true; only the "how much did it ask
for" half is missing, and it is missing because the cluster deleted it.

Reporting a Job that finished before startup means the first flush after a
restart can write a window that has already closed. The spool's age sweep bounds
how far back that can reach, and the payload is idempotent under its key, so a
rewrite is a rewrite rather than a duplicate.

**Not changed.** No existing payload's shape, no RBAC, and no new informer — the
Job handler rides the watch already running for owner resolution. A cluster with
no batch workloads writes nothing at all: a window in which no Job finished
produces no file.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
