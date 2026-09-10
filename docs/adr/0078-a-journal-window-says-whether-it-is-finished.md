# 0078. A journal window says whether it is finished

Date: 2026-09-10
Status: Accepted
Amends: 0027 §4, 0075

Adds `captured_at` to `container_restarts`, `pod_disruptions`, `node_lifecycle`
and `job_runs`, the four payloads that had no way to say whether a window was
final. Closes the open item ADR 0027 §4 deferred and ADR 0075 restated under
*Not addressed here*. Four goldens gain a line, one schema gains a field.
Changes no natural key, no cadence, and nothing about what is collected.

## Context

ADR 0027 §4 recorded the hole and declined to fill it: a journal window's payload
is byte-identical whether it is the window's final value or a slice taken five
minutes in, the backend's wall clock cannot settle it because after an outage the
backlog arrives long after every window has passed, and choosing a shape with no
ingest to ask would be choosing blind. ADR 0075 repeated the deferral and set the
deadline — before a backend stores these kinds, because that is when the cost
changes from a golden diff to a migration. Payloads have travelled since.

Two things about the hole were recorded wrongly, and both were checked against
the code for this decision.

**It spans four kinds, not two.** ADR 0027 §4 and ADR 0075 both name
`container_restarts` and `pod_disruptions`. `job_runs` and `node_lifecycle`
arrived afterwards, took the same envelope — `{kind, source, window_start,
window_seconds, records}` — and inherited the gap without anyone writing it down.

**The choice is narrower than three open options.** ADR 0027 §4 listed a capture
instant, an explicit `window_complete`, and a closed kind mirroring
`usage_window`. Read against what the agent already does, they are not three
peers:

- Every other window kind already answers this question. `usage_snapshot`,
  `usage_window` and `network_window` answer it **per record** with
  `CoveredNanoseconds`, and eleven snapshot-keyed kinds answer it **per payload**
  with `captured_at` — including `restart_counters`, which is `journal`
  provenance. The four journal windows are the only payloads that answer it
  not at all. So the question is which of two existing house answers applies,
  not which of three new fields to invent.
- **Per-record does not apply here.** For usage it earns its place because a
  container that started mid-window genuinely has less coverage than its
  neighbour. A journal window's coverage is the same for every record in it —
  the agent either watched the window or did not — so a per-record field would
  be one value repeated N times. Where journal coverage really is per record it
  already exists: `restart_counters.observed_since`.
- **`window_complete` carries strictly less for the same price.** A boolean
  answers "is it final" and nothing else; `captured_at` answers that and "how
  stale is this slice", which is a real question for anything displaying a
  window still filling. It is also a fact rather than a derivation, which is what
  ADR 0027 §3 defends about this exact field.
- **A closed kind costs four schemas to buy a guarantee already held.** The
  usage pair exists because closed and open have different *content* — the
  closed record is the final aggregate. A journal window's open and closed forms
  differ only in how many records they hold, so a second kind would be a
  byte-identical schema carrying one bit. The one thing it would add — a final
  record that a later slice cannot overwrite — is already true:
  `CloseBefore` removes the window from the accumulator, so nothing is written
  under that key again, and ADR 0072 §4 already forbids reopening a window that
  has ended.

The remaining risk is the one ADR 0027 spent a whole decision on: a timestamp
invites the backend to order by it, and deriving a sequence from the clock was
rejected there as producing "two fields, one value, no answer to which is
authoritative". That is a contract hazard, not a reason to omit the field, and
it is answered where the hazard lives.

## Decision

**1. The four journal window payloads carry `captured_at`.** It is the instant
the agent wrote the payload. `captured_at` at or after `window_start +
window_seconds` means the window is final and nothing more will be added to it;
earlier means the payload is a slice of a window still open, and a later
delivery will replace it under the same natural key.

Comparing two fields of one payload, both stamped by one clock, is what makes
this safe: `window_start` is that same clock truncated to the window grid, so no
skew between machines and no comparison across payloads is involved.

**2. One reading of the clock closes windows and stamps what it wrote.** Each
flush takes `now` once and passes it to both `CloseBefore` and the writer. Taking
the clock twice would let a window be closed against one instant and stamped with
another, which is a payload claiming something the process did not decide.

**3. The contract says what the field is not.** `backend-requirements.md`
requires the backend to take finality from this field, forbids inferring it from
its own clock, and forbids deriving an order from it — supersession stays last
write wins under the natural key, citing ADR 0027. Absent `captured_at` MUST read
as "still open", never as final: absent means an agent too old to state it, and
of the two readings only one is safe to be wrong about.

**4. In the schema it is `optional`.** `node_lifecycle` is the one of the four
with a `.proto` today, and presence is declared field by field there (ADR 0074).
For a timestamp the distinction is load-bearing in a way it is not for a count:
an unset instant read as the epoch is a window that looks open forever, and the
declared presence is what stops that being indistinguishable from a real value.

**5. The cadence machinery already handles it, and that is checked.** The gate's
fingerprint elides a top-level `captured_at` because it "dates the write and not
the state" — so a field that moves every pass cannot make a change predicate
vacuous. All four kinds are `everyPass` today and never consult the gate; the
elision means moving one to `onChange` later stays a one-line change.

## Consequences

**Easier.** A backend can tell a finished hour from one still filling, which is
what it needs before it may treat a window as immutable, roll it into a derived
aggregate, or alert on it. Before this it could only guess, and the honest guess
was to treat every window as provisional forever.

The four kinds stop being the exception among window payloads. Every windowed
payload the agent produces now answers "how much of this did you see", in the
form that suits its shape.

**Harder / given up.** Four goldens moved and one schema grew, which is exactly
the cost ADR 0075 named as the reason to do this before a backend stores these
kinds rather than after. Three of the four still have no `.proto`; when they get
one the field is already in the payload it will describe.

The hazard is now in the protocol: an implementer who orders by `captured_at`
gets something that mostly works, which is the worst kind of wrong. The contract
forbids it in the same paragraph that introduces the field, and that is the only
control available — no agent-side change can prevent a backend from reading a
timestamp as a sequence.

`captured_at` moving every flush means these payloads are never byte-identical
between passes, so a future reader comparing two deliveries sees a diff where
none of the facts changed. The fingerprint elision means the agent already knows
to ignore it; a consumer diffing payloads has to learn the same.

**Not changed.** What is collected, what is filtered, when anything is written or
sent, any natural key, any cadence, the supersession rule, or the shape of any
record inside these payloads. `usage_snapshot` is untouched and always was
outside this: `CoveredNanoseconds` answers the same question per record and
better (ADR 0027 §4).

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
