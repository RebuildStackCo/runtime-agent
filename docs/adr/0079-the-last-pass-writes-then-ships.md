# 0079. The last pass writes, and then ships what it wrote

Date: 2026-09-11
Status: Accepted
Amends: 0068 §4, 0075

Reverses the order of the two halves of shutdown and adds the second one. The
shutdown flush now runs before a final, time-bounded shipping round instead of
after shipping has already stopped. Adds one constant, `FinalRoundBudget`, which
the chart's grace-period floor is now asserted against. Changes no payload, no
natural key, no cadence and nothing about what is collected.

## Context

The controller's shutdown had two halves that could not help each other.

Shipping was a lifecycle task like the watchers: `tasks["ship"] = ship.Run`,
cancelled by the same context the signal cancels. `Shipper.Run` returns on
`ctx.Done()` and `Round` bails on `ctx.Err()`, so from SIGTERM onward the
shipper does nothing at all. Only after `wg.Wait()` — after the shipper has
returned — did `runFlushers(flushers, true)` write the journals and the
inventory that had accumulated since the last periodic pass.

So the agent's last act was to write payloads into a directory nothing would
read again. Not "we do not ship on the way out": *we specifically do not ship
the thing we specifically stayed alive to write.* ADR 0068 §4 sized the grace
period around that flush and called it the reason the floor is 15 seconds.

What makes it loss rather than delay is the volume. The spool is an `emptyDir`
by decision (ADR 0007, ADR 0026), asserted by
`TestTheSpoolIsAlwaysAnEmptyDir`. A container restart keeps it — and since
ADR 0072 and ADR 0077 the next process reads its open windows back. A **pod**
replacement does not: a node drain, a chart upgrade or an eviction takes the
directory with the pod, and everything in it that had not been delivered is
gone.

How much that is depends entirely on whether shipping was configured and
working. With a healthy backend the exposure is small — payloads are written
every minute and offered every thirty seconds, so the backend already holds the
open hour as of a minute ago, and what is lost is the tail plus the shutdown
flush itself. With no backend configured, which is every installation today
(`security.md` §5), the spool is the only copy and a replaced pod loses all of
it. With a backend that has been down, it loses everything since it went down.

The reason the second half was not simply added is the constraint in the way.
ADR 0075 states it: `Run` returns only on cancellation, so a backend that is
down, hostile or absent never reaches the agent's `cancel()`. A final round is
by definition a network call on the exit path, and the whole value of that rule
is that a backend cannot influence the agent's lifecycle. A round that could
block would be a backend holding somebody's node drain open.

## Decision

**1. The shutdown pass writes first and ships second.** `runShutdown` runs the
flushers and then offers the spool once. The order is the decision and it lives
in one named function, tested directly, rather than in the sequence of two
statements at the end of a long one.

**2. The round is bounded by a budget that is not negotiable.**
`FinalRoundBudget` is four seconds for the whole round, from a fresh context —
not the cancelled one the signal produced, because this round exists precisely
because everything else has stopped.

One cap suffices, and this is worth stating because two were considered. `post`
derives its own deadline from the round's, and the earlier of the two wins, so
no request in flight can outlive the budget. A shorter per-request timeout would
buy nothing on top: `Round` already stops at the first payload the backend could
not take, so a stalled request ends the round either way.

**3. The budget is a constant, not a value.** It is the one knob that, set to
sixty by someone reasoning locally, turns a node drain into a five-minute wait
for a backend that is never coming back. Cadences are constants of the protocol
version for the same reason (ADR 0073); this is the same class.

**4. Four seconds, because the floor must not move.** The chart refuses a
controller grace period below 15 (`_helpers.tpl`), and installations that set it
explicitly to 15 would break if the floor rose. ADR 0068 §4 measured the flush
path at about ten seconds; ten plus four is fourteen, so the floor stays where
it is and no existing install is affected.

**5. The floor is asserted against the constant.** ADR 0068 §4 said the grace
period's job was "to be a number that was chosen, and that a later change to the
shutdown path has to be measured against". This is that change, and the
measurement is a test rather than a memory:
`TestTheGracePeriodFloorCoversTheShutdownPath` fails if the budget grows past
what the floor covers. Two numbers that merely agree drift (ADR 0073 §5).

**6. What the round does not do.** It does not run when the shipper is halted on
a 401 or 403 — nothing retries into a rejected credential, and a fleet's worth of
final rounds arriving at a backend that has already said no is the worst version
of it. It does not change delivery semantics: a payload leaves the spool only on
a 2xx, one request per natural key, undelivered files stay. It does not run at
all when no backend is configured, so today's installations write their last pass
exactly as before.

**7. Its result can only be logged, and that is a property of the order, not an
omission.** The coverage payload is written by the flush that runs *before* this
round, so it cannot carry the round's own outcome; writing coverage afterwards
would produce a payload guaranteed never to be sent. Nothing scrapes the metrics
endpoint of a process that is exiting. So the log carries what was delivered,
what was deferred, and whether the budget ran out — the last of which is how
anyone would learn that four seconds was the wrong number.

## Consequences

**Easier.** A graceful pod replacement — a drain, a rolling upgrade, an
eviction, the three ways a controller pod is normally replaced — now delivers
what it just wrote instead of writing it onto a disappearing disk. The shutdown
flush that ADR 0068 sized the grace period around finally has a consumer on the
same path.

The exposure that remains is honest and bounded: what a *hard* kill loses (no
pass runs at all), and what a round that ran out of budget or met a down backend
could not deliver. Both were already lost before this; neither is new.

**Harder / given up.** The agent takes up to four seconds longer to leave, every
time, on installations with a backend configured. That is four seconds of
somebody's node drain, and it is spent whether or not there was anything to
send — a round with an empty spool is one directory listing, so in practice the
cost is only paid when there is something to pay it for.

The exit path now makes network calls, which it never did. The bound is the
whole answer to that, and the bound is tested against a backend that never
answers rather than argued for.

**Not changed.** Any payload, any natural key, any cadence, the one-way
protocol, what is collected or filtered, or the rule that a payload leaves the
spool only when the backend has it. The spool is still an `emptyDir` and still
loses what a replaced pod could not deliver; this decision shrinks that set, it
does not make the volume durable, and nothing here should be read as claiming
otherwise.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
