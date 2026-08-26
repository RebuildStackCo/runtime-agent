# Development Process

How this repository is developed. The rules below are the process; changes to
the process itself go through a PR to this file.

## Branching and commits

- **Trunk-based.** `main` is protected and always releasable. Work happens on
  short-lived branches, merged via PR with squash-merge, so `main` history is
  one commit per change.
- **Conventional Commits.** Commit subjects follow
  `type: summary` — `feat:`, `fix:`, `docs:`, `chore:`, `refactor:`,
  `test:`, `ci:`. This keeps history greppable and makes changelog
  generation mechanical.
- No long-running feature branches. If a change is too big for one PR, it is
  split into steps that each keep `main` releasable.

## Architecture decisions

Decisions that would be expensive to reverse — protocol, storage, security
boundaries, public contracts — are recorded as ADRs in [`adr/`](adr/README.md)
**before or together with** the code that implements them. Reversing one
means writing a superseding ADR, not editing history.

An ADR's body is immutable; its header is not. An ADR that changes part of an
earlier one declares `Amends: NNNN`, and the earlier one gains the mirror
`Amended by: NNNN` — so a reader who opens the old file sees that the ground
moved. `go test ./docs/adr` fails if the graph is one-sided, and prints the line
to add. The rule and its reasoning are in
[ADR 0022](adr/0022-registry-in-code-and-declared-amendments.md).

## Definition of Done

A PR is mergeable when:

1. CI is green (see below).
2. Behavior changes come with tests. No test, no merge — including bug
   fixes, which need a test that fails without the fix.
3. If the change affects what the agent collects, stores, or transmits,
   `docs/security.md` is updated in the same PR. The security document is a
   promise to customers; code must never drift ahead of it. Update it the way
   [ADR 0024](adr/0024-security-doc-states-what-adrs-state-why.md) sets out:
   state what is true and link the ADR for why, rather than restating the
   decision — and mark a claim `[planned]` until it is built. `go test ./docs`
   holds the parts that are lists: the payload table must mirror
   `internal/sink/registry.go` exactly, and every cross-reference must resolve.
4. If the change implements or alters an architectural decision, the ADR
   exists (see above).
5. Everything committed is in English: code, comments, docs, commit messages.

## Testing methodology

- **Golden payload files are the schema contract.** The `local` sink's output
  for known inputs is committed as testdata. A change that alters these bytes
  is a protocol change and must be justified as one — this is the enforcement
  mechanism for the Stage A invariant: the local sink writes exactly what the
  backend sink will transmit.
- **The payload registry is checked against those bytes.**
  `internal/sink/registry.go` is the one list of what the agent ships — kind,
  natural key, delivery discipline, provenance — and tests compare it to the
  goldens in both directions: a kind that ships without a row fails, and a row
  describing nothing that ships fails. The list lived in a document for nine
  slices and drifted in three rows with nothing failing (ADR 0022).
- **Property tests for rollup math.** Rollup merging must be associative and
  commutative, and merging N partial rollups must equal computing one rollup
  over the union. These properties are tested with generated inputs, not
  hand-picked examples — mergeability is what the whole data model rests on.
- **The chart's rendered output is asserted, not reviewed.**
  `internal/chartrender/chart_test.go` renders every install profile and checks
  the promises `docs/security.md` makes about an installation: no write verb in
  the ClusterRole, every cache the agent opens granted, the node bound to
  nothing and holding no API token, no privileged container, exactly the
  capabilities each profile claims, every host mount read-only, the spool an
  emptyDir. It also feeds the rendered configuration to the agent's own strict
  parser, so a chart that would CrashLoop at startup fails here instead
  (ADR 0036). `make chart-lint` adds the helm CLI's own opinion, per profile.
- **End-to-end on kind installs the chart.** The agent runs against a local kind
  cluster with synthetic workloads; assertions are made on the spool contents,
  not on logs. The suites install `charts/runtime-agent` rather than a copy of
  its output, so a broken template fails in the same run as broken code — which
  is the whole reason the raw manifests were deleted rather than kept as a
  reference. The kind node-image matrix pins the oldest supported Kubernetes
  minor (see the README support policy) and a current release.
- **The stability check takes a week and cannot be a CI gate.** kind proves the
  mechanism on a cluster we populated ourselves. It cannot show whether a week of
  ordinary cluster life moves the agent's facts more than the cluster itself
  moved. That is checked on a long-lived cluster: capture the spool twice, seven
  days apart, and compare per workload — the resource envelope, the build, the
  node and zone attribution, the usage windows. Every difference must be
  explainable by something that happened in the cluster.

  Two conditions make the comparison valid, and both follow from decisions made
  elsewhere. The agent runs uninterrupted across the interval: restart counters
  are baselined at startup ([ADR 0020](adr/0020-container-restart-journal.md)
  §5), so a restart moves their origin. And the payloads are copied out at the
  first capture, because the spool sweeps unacknowledged files after
  `DefaultMaxAge` and is a queue, not an archive.

  A record that disappears is not automatically a defect. The Go inventory lives
  only while its workload does
  ([ADR 0018](adr/0018-inventory-records-live-only-while-their-workload-does.md)),
  and revisions are bounded by the Deployment's own history
  ([ADR 0030](adr/0030-deployment-revisions.md)). Each capture's coverage report
  is read beside its payloads, so a shift in what was collected is not mistaken
  for a shift in what is true.
- Unit tests live next to the code (`*_test.go`); e2e lives under `test/`.

## CI gates

Every push and PR runs (`.github/workflows/ci.yml`):

- **Secret scan** (gitleaks) — nothing that looks like a credential enters
  history.
- With the Go scaffolding: `go build`, `go test`, `golangci-lint`.
- **`make chart-lint`** — the Helm chart, once per install profile. helm is
  wired as a Go tool (like kind), so nothing needs installing separately.
- With the protobuf schema: `buf lint` and **`buf breaking`** against `main` —
  the N-2 compatibility promise in `backend-requirements.md` §6 is enforced
  mechanically, not by review vigilance.

## Releases

- Semantic versioning. The payload schema version and the agent version are
  distinct: the schema evolves slower and is governed by the compatibility
  window.
- A release is a tag on `main`; artifacts (image, Helm chart) are built from
  the tag, never from a branch. Release process details will be added when
  the first artifact exists.

## What is intentionally not here

- No fixed sprint cadence or issue ceremony — scope is tracked in GitHub
  issues and milestones, pace is continuous.
- No public roadmap commitments; milestones appear when they are real.
