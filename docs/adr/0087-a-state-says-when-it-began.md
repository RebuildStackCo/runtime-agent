# 0087. A state says when it began

Date: 2026-09-18

Status: Accepted

Amended by: 0088

Amends: 0054, 0067

`sources[].failing` and `shipping.halted` gain a start instant beside them, and
the metrics endpoint gains one gauge. No new collection, no new privilege, no
new read: both instants are already held in memory and discarded at payload
time.

## Context

ADR 0054 made `collection_coverage` the payload the agent answers for itself
with, so that an empty report and a broken agent are not the same bytes. ADR
0067 established that a record carries the instant its source last stated it,
and applied it to the node-fed records — not to the agent's own state.

Every number in that payload is a counter, cumulative from `since`, and two
fields are not numbers at all:

```
sources[].failing   bool
shipping.halted     bool
```

A boolean describes the present, and this kind supersedes — the backend upserts,
last write wins, and one row is all it is obliged to keep. So a state that ended
leaves nothing behind. A watch that failed for nineteen minutes and recovered
produces, on the next flush, bytes identical to a watch that never failed; an
outage shorter than the flush interval is never stated at all. The agent knew
and did not say.

`halted` is the sharper case, and its own field comment already half-admitted
it: a halted agent ships nothing, so the payload carrying the fact never
travels. Its readers are whoever holds the spool and whoever scrapes the metrics
endpoint, and to both of them "shipping stopped" without "at 04:31 nine days
ago" is nearly the same as silence.

The third fact is what makes this cheap. Both instants already exist.
`watchHealth` records where each run of failures began — that is what ADR 0035's
streak is measured from — and `SourceHealths` reduced it to a boolean on the way
out. The halt had no clock at all, because nothing had ever asked it for one.

What is *not* in scope here is history. A state that ended is absent from the
next payload by design, and no field added below changes that.

## Decision

**1. Two states carry the instant they began.** `sources[].failing_since` and
`shipping.halted_since`. Absent is the claim that the agent is not in that
state, so a source that recovers drops the field rather than restating it.

**2. The instant is where the run began, not where it last failed.** What
stopped being true is that the cache was fed, and it stopped when the run
started. The latest failure says only when the agent last looked, which is a
fact about the agent's schedule and not about the source.

**3. The boolean stays for now, derived from the instant.** §6 of
`backend-requirements.md` requires both forms across a deprecation period, and
the schema gate enforces the same: `buf breaking` runs the `FILE` category,
which refuses the deletion of a field even with its number reserved — measured,
not assumed. Both booleans are therefore computed from the instant beside them
in one place each, so no payload can carry a state with no start or a start with
no state.

**4. `synced` stays a boolean and is not amended.** It is a latch rather than a
state that ends: it goes true once and never back. The instant it would carry
for the false case is the payload's own `since` — the cache has not filled since
this process began counting — and for the true case it answers a question about
startup that nothing asks.

**5. One gauge, not two.** `shipping_halted_since_seconds` joins
`shipping_halted`, because for a halt the metrics endpoint is the only channel
that works. `source_failing` gets no companion: its payload arrives on every
pass, and a scrape of the boolean is dated by the scrape.

## Consequences

**Easier.** "When did this stop?" becomes answerable for the two states where it
was not, and answerable from the payload rather than from a customer's logs. The
spool file a halted agent leaves behind stops being a fact about the file's own
timestamp and becomes a fact about the halt.

**Harder / given up.** The payload carries two fields where it carried one, and
will until the booleans are removed — which needs either a released agent to
deprecate against or a decision about the `FILE` gate, and is neither taken nor
pre-judged here. Nothing else grows: both instants were already in memory.

A state that ended is still invisible, and this ADR does not pretend otherwise.
Recovering that needs occupancy within a window, which is a different mechanism
with a different cost, and is not decided here.

**Not changed.** What is collected, what is filtered, what is read, any cadence,
any natural key, or the one-way protocol. No identity of any object reaches
either new field: a source is a resource class, and a halt is about this agent.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
