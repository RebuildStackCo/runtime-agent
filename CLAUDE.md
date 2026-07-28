# CLAUDE.md

Guidance for Claude Code (and any coding agent) working in this repository.

## What this is

`runtime-agent` is the public (Apache 2.0) Kubernetes agent of RebuildStack:
it collects resource-usage rollups, workload metadata, and allow-list-filtered
Go profiles inside customer clusters and ships them one-way to a backend.
All analysis happens outside the cluster; the agent's intelligence is data
reduction, not judgment.

## Read before changing anything

- [`docs/adr/`](docs/adr/README.md) — accepted architecture decisions
- [`docs/security.md`](docs/security.md) — the promise to customers about
  access, data, and filtering
- [`docs/backend-requirements.md`](docs/backend-requirements.md) — the
  agent↔backend contract
- [`docs/development.md`](docs/development.md) — process, testing, CI gates

## Invariants — never violate, never "temporarily" work around

1. **No control channel.** The agent initiates every connection; nothing the
   backend sends changes agent behavior. All configuration comes from Helm
   values via ConfigMap. (ADR 0001)
2. **Sink invariant.** The `local` sink writes byte-identical payloads to
   what the backend sink transmits; analysis consumes only those payloads,
   never internal state. (docs/development.md, golden payload tests)
3. **No monetary fields** anywhere in the protocol or agent code — resource
   units only. (ADR 0004)
4. **Filter early.** "Not collected" beats "collected and dropped" beats
   "collected and not sent". Never collect Secrets or ConfigMap contents;
   drop pod `env`, `args`, `command` at the source.
5. **Loss-harmless state.** Any new persistent state must be recoverable
   from the cluster or the backend; no embedded database. (ADR 0003)
6. **Identities of filtered-out objects never leave the cluster** — only
   aggregate counts per filter type.

## Process rules

- Architectural changes need an ADR (see `docs/adr/README.md`) in the same PR.
- Changes to what is collected/stored/transmitted update `docs/security.md`
  in the same PR.
- Conventional Commits; everything committed is in English.
- Behavior changes require tests; rollup math changes must keep the
  merge-property tests passing.

## Local-only files

`CONTEXT.md` (repo root, gitignored) contains private project notes. Never
commit it, never quote or paraphrase its contents into committed files or
public documents.
