# 0003. Local storage: spool files and a checkpoint, no embedded database

Date: 2026-07-28
Status: Accepted

## Context

The agent needs durable local state: its identity, collector bookkeeping, and
a buffer of payloads not yet acknowledged by the backend. The obvious answer —
an embedded database (SQLite, DuckDB) on the PVC — carries operational cost
that lands on the customer: schema migrations across agent upgrades,
corruption recovery, vacuum/compaction behavior, and a class of bugs we would
have to debug inside clusters we cannot access.

The actual access pattern does not need a database. Payloads are written once,
shipped, and deleted; bookkeeping is a small snapshot.

## Decision

The persistent volume holds exactly three things:

- `identity/` — the private key (born in-cluster, mode 0600) and client
  certificate.
- `state.pb` — a single protobuf checkpoint of collector bookkeeping,
  rewritten atomically (write to temp file, fsync, rename). Loss is harmless:
  the agent re-derives state from the cluster.
- `spool/` — completed payload batches as files. Filenames are the natural
  keys (cluster, kind, time window), so the spool needs no index. Deletion
  is the acknowledgment: a file exists exactly as long as its data is
  unconfirmed by the backend.

No embedded database. The design rule for any new piece of state: it must
answer "what happens if this is lost?" with "nothing that the backend or the
cluster cannot restore."

Explicitly **not** on the volume: the raw sample ring buffer (emptyDir —
raw data must not survive the pod), any cached copy of configuration (the
ConfigMap is read at start; if unavailable, the agent does not collect), and
long-term rollup history (the backend is the source of truth).

## Consequences

- Upgrades have no migration step; recovery from a corrupt file is "delete
  it" — worst case the spool re-ships acknowledged-but-undeleted data, which
  the backend deduplicates by natural keys.
- The spool doubles as the Stage A deliverable: with no backend configured,
  the `local` sink simply leaves the same payload files in place for
  inspection — same schema, same bytes the backend sink would transmit.
- If collector bookkeeping ever outgrows a single snapshot file, the
  designated escape hatch is bbolt (a single-file B+tree, no server, no SQL) —
  a new ADR, not a silent change.
