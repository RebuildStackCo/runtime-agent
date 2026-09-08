# 0075. A payload leaves the spool only when the backend has it

Date: 2026-09-08
Status: Accepted
Amends: 0027 §2

Adds the transmit path: one configuration setting, an HTTP POST per payload, and
the deletion rule that makes the spool a queue rather than a directory. Closes
the obligation ADR 0027 §2 recorded against a shipper that did not exist. Adds a
`shipping` block to `collection_coverage` and a `backend_configured` switch to
its configuration shape. Changes no other payload and no natural key.

Deliberately not here: TLS, certificates, enrollment, renewal, CA pinning, and
any field naming the sender. Those are ADR 0005 and ADR 0008, and this decision
is arranged so that none of them has to be torn back out of a schema later.

## Context

A backend exists and receives payloads. This agent has no transmit path at all:
`internal/sink` writes spool files and that is where they stay. Twenty-two
payload kinds, four documents of contract, and nothing has ever travelled
between the two.

Two systems built toward one document and never connected diverge in the places
the document does not cover, and the only thing that finds those places is a
running agent talking to a running backend. That is what this change is for. It
is a first-contact stage on infrastructure the operator controls, over plain
HTTP, and the transport arrives as its own piece of work.

**Identity is the thing most easily faked here and hardest to remove.** A
cluster identifier in a payload, or a header naming the sender, would work today
and would have to be torn out of the schema, the handlers and the stored keys
when mTLS makes identity a property of the connection (ADR 0005). So there is
none: the backend attributes everything to one configured source and knows it.
An empty space is easier to fill correctly than a temporary shape is to remove.

**The spool is already the queue.** Its filenames are natural keys, it holds one
file per key, and deletion is acknowledgment (ADR 0003). A shipper therefore
needs no state of its own that a restart would have to recover, which is what
keeps invariant 5 intact without an embedded database.

**The obligation from ADR 0027 comes due.** Removing the `sequence` field moved
the ordering guarantee from the backend to the agent: at most one request per
natural key in flight. It was written into `backend-requirements.md` §4 before
anything could honour it. This is where it has to become code.

**Two ceilings are unsettled, and pretending otherwise would be worse than
saying so.** §4 requires payload size and rate limits to be explicit in the
protocol, and there is nowhere yet for them to be stated. Nothing in this change
settles them.

## Decision

**1. One setting: the backend's base URL, and no default.** Absent means ship
nothing, which is exactly today's behaviour, so an agent that is not configured
for a backend keeps working precisely as it does now. No credential, no token,
no `insecure` switch that would later mean something else. `POST
{base}/v1/kubernetes/{kind}`, `Content-Type: application/json`, one payload per
request, the body the spool file verbatim. No batch endpoint and no envelope.

The `{kind}` segment is the registry name read out of the payload's own bytes
and checked against the registry (ADR 0022). A file whose kind has no row is not
a URL this agent will construct.

**2. A spool file is deleted only after a 2xx, and only if it is still the file
that was sent.** A superseding writer replaces a file by rename while its
predecessor is in flight; deleting by name alone would drop a newer payload that
was never offered, and nothing downstream could notice. The identity of the
file — not its name — is what the delete is conditioned on.

**3. The classes, and what each does:**

| Answer | What happens |
|---|---|
| 2xx | Delivered. The file goes. |
| 429, 5xx, network error, timeout | Deferred. The file stays, the round ends, the backoff grows. |
| 400 | Held: counted `malformed`, never offered again by this process. |
| 413, or past the agent's own ceiling | Held: counted `too_large`, never offered again by this process. |
| 401, 403 | Counted `unauthorized`. Shipping halts for the life of the process. |
| anything else | Deferred, as above. |

**4. A permanently rejected payload is held, not deleted.** This is the one
place the change departs from "deleted only after a 2xx" having a single
meaning, and it departs in the conservative direction: the payload is marked in
memory, never sent again, counted, logged, and left in the spool until the
sweep's age bound removes it like anything else (ADR 0042).

Dropping it immediately was the alternative. Both lose the payload — the spool
is bounded and nothing re-reads it — so the choice is not between keeping and
losing but between losing it now and losing it in a day. What the day buys is
the bytes: a 400 means the agent and the backend disagree about a schema, and
finding exactly that is what this whole stage exists for. A payload discarded at
the moment of rejection takes the evidence with it. `backend-requirements.md` §5
already permits either ("drop-and-log or hold-and-alert; never blind retry"),
and the mark is keyed by file identity, so the next pass writing a new payload
under the same name clears it and the new one is offered.

**5. An identity failure stops shipping and does not retry.** There is nothing
to retry into, and a fleet retrying into an authentication failure is the
classic way it takes down the thing it is trying to reach. The halt lasts the
life of the process; collection, spooling and eviction are untouched. In
`cmd/agent` the shipper is a lifecycle task and a task that returns cancels the
agent, so a halted shipper blocks until shutdown rather than returning — the
agent must not stop collecting because a backend refused it.

**6. One goroutine, one request in flight.** That is the whole implementation of
ADR 0027 §2: the spool holds one file per natural key, so a shipper that never
has two requests open never has two versions of a key in flight, and an older
snapshot cannot arrive after a newer one. It also bounds what one agent costs a
recovering backend. Rounds go oldest-first, which is the sweep's own eviction
order: what is closest to being dropped is closest to being delivered.

**7. Backoff is exponential with jitter, and half of each delay is spread.**
Five seconds doubling to five minutes, and the delay actually taken is drawn
from the top half of that window. After an outage every agent in a fleet has a
full spool and the same idea at the same moment; the backend is required to
tolerate a catch-up burst, and the agent is required not to make it a stampede.

**8. Nothing is read out of a response but its status.** The body is drained so
the connection can be reused and discarded unparsed. ADR 0001 forbids the
backend to send anything the agent acts on; this is the line where that stops
being a promise about the backend and becomes a property of the agent.

**9. The size ceiling is a constant of 4 MiB, and it is a placeholder.** A
payload past it is refused before the request and counted as `too_large`,
reaching a 413's outcome without spending the round trip. The number is chosen
to sit under the limits proxies and gateways commonly impose rather than to
describe anything either side has measured.

It is not a chunking threshold and must not become one: a superseding payload is
the complete state under its key and the agent never splits one (ADR 0027,
`backend-requirements.md` §4). What it is waiting for is a stated protocol
limit. When there is one it belongs in the registry as a property of a kind,
beside cadence — a size limit is the same sort of fact as a send interval, and
ADR 0073 already built the place for it. Until then the counter is what makes
the constant's wrongness visible: `too_large` rising with nothing else wrong is
a cluster producing payloads this number does not fit.

**10. `collection_coverage` gains a `shipping` block**, with `delivered`,
`deferred`, `unreadable`, `halted` and a nested `rejected` carrying the three
reasons. It does not reuse `intake_rejections`, which carries the same three
words about the opposite direction — what this agent's own receiver refused from
a node inside the cluster (ADR 0067). Merging them would leave neither
answerable, and the existing `/metrics` series for the node channel would
silently start counting backend refusals.

The block's subject bounds what it can say: a report that arrived was shipped,
so it carries the history of the payloads before it and never the delivery that
brought it. A halted agent ships nothing at all, so `halted` reaching a backend
describes a halt that came later. Its readers there are whoever holds the spool
and whoever scrapes `/metrics`, which is why the fact is in both.

## Consequences

**Easier.** The agent does the thing it was built for. The spool stops being a
directory that only grows to its bounds and becomes a queue that drains. Every
divergence between these payloads and an ingest that has never seen them becomes
findable, which is the point.

`backend-requirements.md` §4's ordering obligation is now met by construction
rather than promised, and §5's error classes have an implementation to point at.

**Harder / given up.** A payload the backend refuses permanently is lost within
a day, and the agent cannot re-derive it — the same property every payload in
the spool has always had (ADR 0007). The `too_large` counter is the alert for
both halves of that, and a cluster whose payloads do not fit 4 MiB will show it
there before anything else.

Sequential delivery means throughput is one payload per round trip. A spool with
a large backlog drains slowly, and after a long outage the oldest payloads leave
on the age bound before the newest ones are sent. That is the correct loss —
oldest-first is what makes it — but it is loss, and the alternative is
concurrency this decision refuses on ADR 0027's account.

A halted shipper is silent from the backend's side: it cannot deliver the
payload that would say so. Only the operator's own monitoring sees it, which is
what `shipping_halted` on `/metrics` exists for.

**Not changed.** What is collected, what is filtered, what is written, or when.
Collection does not depend on shipping in any direction: an unreachable, slow or
hostile backend changes nothing about what this agent does with its cluster, and
the tests hold that line explicitly.

**Not addressed here.** The two journal kinds still have no completeness signal
— `container_restarts` and `pod_disruptions` are byte-identical whether a window
is final or sampled part way through, which ADR 0027 §4 recorded as belonging to
this slice and deferred against an ingest that could be asked. There is an
ingest now, and the question is what it wants to do with the signal; that is a
conversation and a payload change, not a property of the transmit path. It must
land before a backend stores these kinds, because that is when the cost changes
from a golden diff to a migration.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
