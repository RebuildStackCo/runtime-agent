# 0083. The agent names itself in the request it makes

Date: 2026-09-16

Status: Accepted

Every request the agent sends carries `User-Agent: runtime-agent/<version>`.

## Context

The agent states its version in exactly one place: inside `collection_coverage`,
as `agent.version`. That is a payload — a document about the cluster — and the
version rides in it as a field about the sender.

So anything wanting to know which build made a request must open a body and
interpret it to find out. Three costs follow, and none of them is about the
receiver's convenience:

- The answer arrives on that payload's cadence. A delivery of any other kind
  states nothing about what sent it, and between two coverage flushes the
  question has no answer at all.
- It is available only to a reader that already parses payloads. An access log,
  a proxy, a tcpdump — everything that sees requests rather than documents —
  sees a sender that will not say what it is.
- A fact about the sender is coupled to the schema of a fact about the cluster.
  They change for unrelated reasons.

A version is not a fact about the cluster. It describes what software made this
request, which is the question `User-Agent` has always answered.

## Decision

**1. The product and its version, on every request.** `runtime-agent/<version>`,
named after this repository and the chart. The value is fixed when the process
starts, because it cannot change while it runs, and it is the same on every
request rather than on some of them — a header that appears only sometimes is
harder to reason about than one that is always there.

A standard header rather than one of ours. An `X-` name is likelier to be
dropped by an intermediary, and it is one more name for an operator to learn
before they can read their own traffic.

**2. A version that cannot be stated is not stated.** The build stamps it from
`git describe`, so its content comes from a tag, and a tag is a string this
repository does not control. What may be stated is bounded here: at most 64
characters, of what a semantic version is made of plus the letters that spell
the `dev` an unstamped build carries.

Outside that the product travels alone — `runtime-agent`, naming this agent and
claiming nothing about which build it is. The alternative, sending the value
anyway, is worse than it looks: a tag holding a space or a `/` produces
something that is not a version in the shape the header agreed to, and a tag
holding a CRLF does not produce a bad version but a second header. Bounding it
at the source means no reader has to be the one that notices.

Half an answer is the right answer here because a missing version is a case any
reader must already handle — an agent built before this header existed sends
none, and an intermediary may strip one that was sent.

**3. Nothing new leaves the cluster.** `collection_coverage.agent.version`
already carries this exact string, from the same variable. What changes is the
channel, not the data, which is why this adds no promise to
[`security.md`](../security.md) beyond naming where the existing one now also
appears.

**4. The payload keeps its field.** Not a duplication to be cleaned up later:
the `local` sink writes byte-identical payloads to what the backend sink
transmits (CLAUDE.md invariant 2), and a header is not part of a payload. Moving
the version out of the body would make it invisible to everyone reading a local
sink, which is the configuration where no request is made at all.

## Consequences

**Easier.** A request says what sent it, to anything that can see a request. The
answer no longer waits for a coverage flush, and it is there on the first
delivery a new build makes rather than on the first coverage payload it writes.

**Harder / given up.** The version is now stated in two places. Within one
process they cannot disagree — both read the same `version` variable — but a
reader has to know which it is looking at, and the two travel on different
schedules: the header on every request, the field on the coverage cadence.

A tag that cannot be stated becomes invisible rather than loud. The agent knows
it declined, and says so nowhere: a warning on a path this hot would repeat on
every round, and a counter for it would be a payload field reporting on a header
that payload does not carry. The build that tagged itself that way is the place
the problem is, and `git describe` is where it can be seen.

**Not changed.** No control channel is created or implied (ADR 0001): the header
is outbound, and nothing in any response changes what the agent does. No new
read, no new privilege, and no new fact about the customer's cluster — the
string was already being sent.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
