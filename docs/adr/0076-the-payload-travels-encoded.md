# 0076. The payload travels encoded, and the encoding is not what it is

Date: 2026-09-10
Status: Accepted

Gzips every request body the shipper sends and declares it with
`Content-Encoding: gzip`, unconditionally and without negotiation. Adds
`payload_bytes` and `transmitted_bytes` to the `shipping` block of
`collection_coverage`, and the same two numbers to the metrics endpoint. Changes
no payload, no natural key, no cadence and nothing about what is collected.

## Context

The transmit path landed in ADR 0075 as the simplest thing that could work: read
the spool file, POST the bytes, delete on a 2xx. It sends them uncompressed.
Two properties of what it sends make that expensive out of proportion to any
other number in the agent.

**The payloads are indented JSON.** `Spool.write` encodes with
`json.MarshalIndent(payload, "", "  ")`, and the indentation is deliberate: a
spool file is read by a person looking at what the agent decided to send, and
the golden tests compare it as text. So every record carries its field names in
full, twice-indented, once per record.

**The largest kinds are re-sent whole every minute.** `usage_snapshot`,
`process_counters` and the four journal windows are `everyPass` in the registry:
Floor and Ceiling both one minute, `TriggerAlways`. An open window's payload is
rewritten and re-shipped sixty times before the hour closes, each time carrying
every record accumulated so far. Superseding ingest discards fifty-nine of the
sixty.

Measured on a synthesised `usage_snapshot` of a thousand rollup keys — the shape
a cluster of a few thousand pods produces, since replicas collapse into one
record per (namespace, workload, container) under ADR 0006:

| | bytes | ratio | CPU |
|---|---|---|---|
| as sent today | 1 129 738 | — | — |
| gzip level 1 | 133 308 | 8.5× | 3.3 ms |
| gzip level 6 | 99 092 | **11.4×** | 8.7 ms |
| gzip level 9 | 93 260 | 12.1× | 37.5 ms |

Compact JSON without indentation is 38% smaller on its own and only 1.06× better
than level 6 once gzip has run, which does not pay for moving every golden byte
in the repository.

Every payload kind in `testdata` compresses: the smallest, a 285-byte
`ebpf_profile`, still reaches 1.37×, and none expands. There is no size below
which the encoding is a loss, so there is no threshold to choose and no branch
to write.

The obvious objection is that this is a negotiation: `Accept-Encoding` exists,
and a well-behaved client asks. This client cannot ask, and the reason is the
first invariant in the system. ADR 0001 forbids the backend to tell the agent
anything the agent acts on, and a capability reply is exactly that. So the
encoding joins the cadences of ADR 0073 as a constant of the protocol version:
published in a table the backend reads for the agent version it already records,
never sent on the wire.

## Decision

**1. Every request body is gzip at level 6, and every request says so.** The
header is `Content-Encoding: gzip` on every payload of every kind. There is no
uncompressed mode, no configuration value and no negotiation. A backend that
cannot decode gzip cannot ingest from this agent, which is stated in
`backend-requirements.md` §3 rather than discovered.

Level 6 rather than 1 or 9: 1 gives up a quarter of the compression to save five
milliseconds a minute, and 9 spends four times the CPU for six percent. Neither
trade is close.

**2. The payload is what is encoded; the encoding is how it travels.** This is
the sink invariant restated on the wire, and it is what keeps that invariant true
now that the wire and the disk no longer hold the same bytes: `gunzip` of a
request body is the spool file byte for byte. The `local` sink is unchanged
because there is nothing to change — it writes the payload, and the payload is
what the backend receives.

The test that asserted the body was the spool file verbatim now decodes it
first, which makes the invariant a round trip rather than an identity. That is a
stronger check than the one it replaces: an identity could hold by accident if
both sides were wrong the same way, and a round trip cannot.

**3. Everything else in the contract describes the decoded bytes.**
`MaxPayloadBytes` stays where it is, on the file the shipper read, not on what it
sent — the ceiling is a claim about what a backend must be able to hold, and it
would move with the entropy of a cluster's data if it were measured after
encoding. The contract says the same to the other side: apply size limits after
decoding, and never infer a payload's size from its request's.

**4. The agent reports both sides of it.** `payload_bytes` and
`transmitted_bytes` count every attempt that reached the wire, whatever the
answer was, so a retried payload is counted each time it cost a request. Their
ratio is the compression achieved on that cluster's own data.

This is the only number that can say the decision was right for an installation
rather than for the measurement above, and it is the number that would show it
going wrong — a ratio near 1 means payloads full of incompressible content, which
would be a fact about a kind worth knowing for its own sake.

## Consequences

**Easier.** The egress of an installation drops by roughly an order of magnitude
with no change to what is collected, what is sent, how often, or what any
consumer of a payload sees. Nothing downstream is aware of it: a backend that
decodes the body reads exactly the bytes it read before.

The size ceiling now bounds a payload that costs a tenth of its size to deliver,
so the cluster at which `too_large` starts firing is unchanged while the cost of
reaching it is not.

**Harder / given up.** A request body is no longer readable in a packet capture
or a proxy log without decoding it. In-cluster that changes little — the traffic
was already one-way to one configured address — but it does mean an operator
debugging an ingest problem needs `gunzip` where they previously needed nothing.
The spool file remains plain text, and it is the same bytes, so the readable
artefact still exists where it always was.

The agent spends CPU it did not spend: 8.7 ms per megabyte of payload, once per
flush. Against ADR 0068's rule that the agent yields first this is not close to
material, and if it ever became so, level 1 recovers most of the saving for a
third of the cost without changing anything else in this decision.

A backend that silently ignores `Content-Encoding` will read gzip bytes as JSON
and answer 400, which the agent treats as permanent for that payload and holds
(ADR 0075). That is the correct failure — loud, per-payload, and with the
evidence left in the spool — but it is a failure that did not exist before, and
it is why the obligation is written in the contract rather than assumed.

**Not changed.** What is collected, what is filtered, what is written to the
spool, when anything is sent, any natural key, any cadence, the one-way
protocol, or the shape of any payload. `usage_snapshot`'s sixty writes an hour
are still sixty; they are a tenth of the bytes.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
