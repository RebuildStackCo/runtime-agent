# 0073. A payload kind declares how often it is sent, and the ceiling is what makes that safe

Date: 2026-09-07
Status: Accepted
Amends: 0012, 0030 §7, 0034 §7

Adds a cadence to every registry row. Changes no payload's contents and no
natural key. Amends ADR 0012's "metadata is flushed on the coverage cadence",
and the shared capture instant ADR 0030 §7 and ADR 0034 §7 decided.

## Context

Nothing ships yet. `internal/sink` is a spool to disk and there is no transmit
path at all. This decision is taken before that path is designed because it
belongs to the contract rather than to the transport: how often a kind is
offered is a fact the backend has to be able to rely on, and there is no channel
that could tell it (ADR 0001).

Today every superseding payload rides one ticker — `coverageInterval`, a minute
— and the whole flusher list rides it with it. For `collection_coverage` that is
right: it is small, and its freshness is the only thing that says the agent is
alive (ADR 0054 §5).

For the structural snapshots it is not, and the numbers are not marginal.

- `node_metadata` is ~1.5 KB per node in the golden. A two-hundred-node cluster
  is ~300 KB a flush and ~433 MB a day, for fields — instance type, zone,
  capacity, kubelet version — that change perhaps once a day.
- `workload_metadata` is worse: ~1.3 KB per container record, so a cluster with
  two thousand collected containers is ~2.6 MB a flush and ~3.7 GB a day.

Storage on the backend is unaffected either way: both kinds are keyed one row
per cluster and superseded in place (ADR 0027). The entire cost is network and
write amplification.

Three facts shape what can be done about it.

**A superseding kind has no ordering field**, by decision (ADR 0027). The spool
holds one version of a natural key, atomically replaced, and last-write-wins is
correct because there is never a second version to rank. The consequence for
sending less often is sharp: a change that is *missed* — a restart that lost the
comparison state, a predicate that was wrong, a payload whose shape moved under
one — would stand as the answer forever, and nothing on either side could
notice. Sending on change alone is not a safe scheme; sending on change with an
unconditional ceiling is.

**The payload is indivisible.** A partial trigger must never become a partial
payload: `supersedes` means the last write is the complete state, and a delta
would break that and everything the backend builds on it. One changed field on
one node sends every node.

**Some payloads never hold still.** `workload_metadata` carries `pod.phases`,
`pod.nodes` and replica counts, which move on every rollout and every
reschedule. `go_inventory` carries a coverage block that dates each node's
latest report, so it differs on nearly every pass however still the fleet is.
A byte comparison over either degrades straight back to sending every minute.

## Decision

**1. Cadence is a property of a payload kind, declared in the registry.** It
sits in `internal/sink/registry.go` beside the natural key, the delivery
discipline and the provenance, because that table is already the single list of
what this agent ships and the backend's documentation treats it as normative
(ADR 0022). Three values: a **floor**, the shortest gap between two writes of
one key; a **ceiling**, the longest, unconditional; and a **trigger**, what
decides the writes in between.

The ceiling is the load-bearing part. It turns "forever" into "at most one
ceiling", and it is what makes change-triggered sending safe rather than clever.
The floor is the debounce as much as the rate limit: a cluster autoscaler adding
ninety nodes in twenty seconds must produce one send at the next pass, not
ninety.

**2. The table.** Three triggers and four cadences:

| Kind | Floor | Ceiling | Trigger |
|---|---|---|---|
| `collection_coverage` | 1 min | 1 min | always |
| `usage_snapshot` | 1 min | 1 min | always |
| `container_restarts` | 1 min | 1 min | always |
| `pod_disruptions` | 1 min | 1 min | always |
| `job_runs` | 1 min | 1 min | always |
| `node_lifecycle` | 1 min | 1 min | always |
| `process_counters` | 1 min | 1 min | always |
| `node_metadata` | 1 min | 15 min | changed |
| `workload_metadata` | 5 min | 15 min | changed |
| `workload_revisions` | 5 min | 15 min | changed |
| `workload_policy` | 5 min | 15 min | changed |
| `cluster_policy` | 5 min | 15 min | changed |
| `restart_counters` | 5 min | 15 min | changed |
| `go_inventory` | 5 min | 15 min | changed |
| `process_peaks` | 5 min | 15 min | changed |
| `listening_ports` | 5 min | 15 min | changed |
| `usage_window`, `network_window` | — | — | event (its window closed) |
| `oom_kill`, `go_build` | — | — | event |
| `ebpf_profile`, `pprof_profile` | — | — | event (a capture finished) |

Four of those rows were decided against the obvious value:

- `collection_coverage` is never held back. Its freshness *is* the liveness
  signal, so a suppressed coverage payload is the agent reporting itself dead.
- `process_counters` is never held back either, for a different reason: alone
  among the superseding snapshots its records are differences, not readings
  (ADR 0062). A skipped reading is a reading that is still true; a skipped
  difference is an interval nothing else carries.
- `node_metadata` keeps the one-minute floor, because it earns it. A node's
  conditions carry their last *transition* and not the kubelet's heartbeat, so
  the payload genuinely holds still between real changes — confirmed in
  `internal/model`, not assumed — and a minute of debounce costs a steady
  cluster nothing while getting a new node described within one.
- `workload_metadata` takes the five-minute floor instead of a projection past
  its volatile fields. See §3.

**3. The comparison is the payload's own bytes, with its capture instant elided,
and nothing else.** `captured_at` dates the write and not the state, so it
differs on every pass by construction and comparing it would make every
predicate vacuous. It is elided at the top level only: a field of that name
nested inside a record is a fact about the record.

Nothing else is elided, and that is the decision, not an omission. A projection
that skips a noisy field is a set of fields whose changes go unseen for a
ceiling, and one that has to be maintained by hand every time a payload grows —
a new field added and forgotten in the projection is invisible to every test,
because the field is legitimately in the payload. Where a payload is too noisy
for a one-minute predicate, the answer is a longer floor, which cannot be wrong
in that way. A longer floor is never worse than an unconditional send at the
same interval, so a predicate that rarely fires costs nothing but its own hash.

**4. The decision is made before the spool write, not before transmission.**
The spool *is* the queue: its filenames are the natural keys and deletion is
acknowledgment (ADR 0003). A payload it holds is a payload to send, so a
predicate on the far side would leave the transmit path either deleting files it
never sent — making deletion mean something else — or leaving them to age
against the spool's own bounds (ADR 0042). Deciding at the write also keeps the
local sink honest: what is in the spool is what ships, which is the invariant
the golden payloads rest on.

What this gives up is small and worth naming: for a kind keyed one per cluster
the spool holds exactly one file, replaced in place, so there is no local history
to coarsen. The single file is now up to a ceiling stale rather than up to a
minute stale. That is the whole cost.

**5. The declaration and the behavior are held together in both directions.**
The gate reads the registry row, so there is no second copy of the number to
drift. What a test adds is the two things that reading cannot check:

- Per change-triggered kind, over the writers that actually ship: an unchanged
  payload is held back past its floor, a changed one lands at it, a change
  inside the floor waits, and an unchanged one is rewritten at the ceiling
  (`internal/sink/cadence_test.go`). A row the table there does not cover fails.
- For the fixed cadences, the number in the registry against the ticker that
  actually writes them (`cmd/agent/cadence_test.go`). The sink never sees those
  tickers, so nothing else could catch one moving alone.

Suppression is counted per kind and exposed at `/metrics` (ADR 0070). A
change-triggered kind that never suppresses anything is a predicate that is not
working, and that is the only place it shows.

**6. Two payloads of one flush may now carry different capture instants.** ADR
0030 §7 and ADR 0034 §7 made revisions and restart counters share
`workload_metadata`'s instant so that a consumer could not pair a revision with
a shape from a different moment. Gated per kind, they will diverge.

What made the join sound is not the shared number, and now says so directly: a
superseding snapshot that was not rewritten was not rewritten *because nothing
in it changed*, so it still describes the current moment. A snapshot's
`captured_at` is when the state it carries was last observed, not the last
moment it was true. The backend obligation this creates — read a superseding
payload as current until the next delivery of its kind — is stated in
`docs/backend-requirements.md` §4, and the ceiling is what bounds it.

**7. Cadence, maximum payload size and request rate are one class, and only one
of them has numbers today.** All three are constants the agent must know without
being told, because there is nothing that could tell it (ADR 0001); all three
belong to the published protocol version, which the backend already records per
agent (§6 of the contract); and the registry is where the per-kind ones live.
Cadence gets its numbers here because it is enforced here. The other two do not:

- A stated limit is a measured limit (ADR 0039). There is no transmit path, so
  there is nothing to measure, and a byte ceiling chosen now would be a number
  the agent could neither honor nor check.
- The purpose the contract gives the size limit — "so the agent can chunk
  deterministically" — does not describe this agent. A superseding payload is
  indivisible by §1 of this decision and ADR 0027, so a `workload_metadata` too
  large to send cannot be split and stay within the contract. The size limit's
  real job is to tell an operator and a backend what a cluster of a given size
  will offer, which the cadence table plus a per-kind record size now makes
  computable. The contract is corrected to say that, and to record that
  chunking, if it is ever needed, is its own decision.

## Consequences

**Easier.** The two payloads that dominated the wire stop dominating it. A
steady two-hundred-node cluster sends `node_metadata` four times an hour instead
of sixty — 433 MB a day becomes 29 MB — and `workload_metadata` likewise, or
five times an hour through a continuous rollout. A backend can compute what a
cluster of a given size will offer per day from the registry alone, before
writing a line of ingest.

**The cadence across a fleet is the oldest agent installed.** A protocol
constant is not negotiated, so an installation running last year's chart sends
at last year's cadence and nothing can ask it not to. That is the price of
having no control channel, and it is worth stating plainly here rather than
discovering it when a fleet is large: changing a number in this table changes
nothing already deployed, and the backend must tolerate every cadence any
supported agent version declares.

**A missed change now costs up to one ceiling.** Before, a superseding payload
was rewritten every minute, so a wrong answer corrected itself within one. Now
the correction is bounded by fifteen minutes for every change-triggered kind.
Fifteen was chosen against that: long enough to make the saving the point of the
exercise, short enough that a wrong snapshot is not a wrong report.

**`go_inventory`'s floor does the work its predicate cannot.** Its coverage block
dates each node's latest report, so the payload differs on nearly every pass and
the predicate almost never suppresses anything. The five-minute floor still cuts
it fivefold, and the visible cost is that `asserted_at` — how a reader bounds a
record's age (ADR 0067) — is now up to five minutes coarser. Splitting the
coverage block out of the payload would fix this properly and is not this
decision: it changes what a payload contains.

**`workload_metadata` still sends every five minutes on a busy cluster.** The
long floor is a fivefold cut, not a fifteenfold one, and on a cluster in
continuous rollout that is all it is. The proper fix is to move `pod.phases`,
`pod.nodes` and the replica counts into a kind of their own, so the shape and
the placement are sent at the cadences each deserves. That is a payload change
and a separate decision; this one deliberately leaves the payloads alone.

**The end-to-end suite got slower, and there was no way to make it not.** An
e2e test creates a fixture in a live cluster and polls the spool for it, so its
budget is now a floor plus the informer's lag: eight polls across five files
were raised by five or six minutes each, and five suite timeouts with them. Shortening the
floor for a test installation was the obvious escape and is not available — a
cadence an installation can set is a cadence the backend cannot rely on, which
is the whole of §1.

**The shutdown pass respects the floor.** A graceful stop writes the journals and
the inventory one last time (`runFlushers`), and a change-triggered snapshot
inside its floor at that moment is not written. What is lost is a superseding
snapshot the backend already holds a version of, no older than one floor, which
a restarted agent republishes on its first pass — the gate is in memory, so a
fresh process has nothing to compare against and writes. The journals, which are
what the shutdown pass exists for, are on the fixed cadence and unaffected.
