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

## Definition of Done

A PR is mergeable when:

1. CI is green (see below).
2. Behavior changes come with tests. No test, no merge — including bug
   fixes, which need a test that fails without the fix.
3. If the change affects what the agent collects, stores, or transmits,
   `docs/security.md` is updated in the same PR. The security document is a
   promise to customers; code must never drift ahead of it.
4. If the change implements or alters an architectural decision, the ADR
   exists (see above).
5. Everything committed is in English: code, comments, docs, commit messages.

## Testing methodology

- **Golden payload files are the schema contract.** The `local` sink's output
  for known inputs is committed as testdata. A change that alters these bytes
  is a protocol change and must be justified as one — this is the enforcement
  mechanism for the Stage A invariant: the local sink writes exactly what the
  backend sink will transmit.
- **Property tests for rollup math.** Rollup merging must be associative and
  commutative, and merging N partial rollups must equal computing one rollup
  over the union. These properties are tested with generated inputs, not
  hand-picked examples — mergeability is what the whole data model rests on.
- **End-to-end on kind.** The agent runs against a local kind cluster with
  synthetic workloads; assertions are made on the spool contents, not on
  logs. A long-lived dogfood cluster complements this later.
- Unit tests live next to the code (`*_test.go`); e2e lives under `test/`.

## CI gates

Every push and PR runs (`.github/workflows/ci.yml`):

- **Secret scan** (gitleaks) — nothing that looks like a credential enters
  history.
- With the Go scaffolding: `go build`, `go test`, `golangci-lint`.
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
