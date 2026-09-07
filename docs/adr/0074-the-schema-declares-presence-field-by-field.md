# 0074. The payload schema is written for three kinds, and presence is declared field by field

Date: 2026-09-07

Status: Accepted

Amends: 0002

## Context

ADR 0002 chose protobuf and said the `.proto` files are the contract between
agent and backend. They were never written. What exists instead is
`internal/sink`, which writes hand-rolled JSON, and twenty-two golden payload
files that the sink's own tests hold byte-for-byte. A backend has been read
against those bytes, on the strength of this repository's invariant that the
`local` sink writes exactly what the transmitting sink will transmit. So the
corpus became the contract by default, without anyone deciding it should be.

The wire format is the least reversible thing either side owns.
`backend-requirements.md` §6 gives a breaking change an N-2 support window and a
deprecation period during which both forms are accepted. Right now not one agent
is installed anywhere, so the window costs nothing and the deprecation period is
empty. This is the last cheap moment to get the shape right.

**Proto3 singular scalars have no presence.** A zero and an unset field are the
same bytes on the wire and the same absence in canonical JSON. That erases the
distinction this contract is built on. `collection_coverage` ships

    "intake_rejections": { "unauthorized": 1, "too_large": 3, "malformed": 0 }

and `"malformed": 0` is a claim: nothing was refused as malformed. Declared as a
plain `int64` it disappears, and an agent that measured zero becomes
indistinguishable from an agent that said nothing about malformed rejections.
The same erasure turns "this machine's size was not stated" into "this machine
had no CPUs", which a consumer then adds to a fleet total.

**Three payload kinds have been read closely; the other nineteen have not.**
Reading a payload is what finds defects in it — a kind recorded under the wrong
provenance, an empty window reported as an unknown one — and both of those were
caught by someone working through that kind's data, not by someone transcribing
a struct. A schema written for a payload nobody has studied freezes a guess for
an N-2 window.

## Decision

**1. Three kinds: `node_metadata`, `collection_coverage`, `node_lifecycle`.**
Not the other nineteen. Adding a kind to the schema means reading its data
first; the schema growing kind by kind is the intended pace, not a backlog.
`internal/pb/kubernetesv1`'s test names the covered kinds and logs the
uncovered ones, so the gap is a number in test output rather than an assumption.

**2. Presence is declared field by field, by asking one question of each: do
silence and nought mean the same thing here?** Where they do not, the field is
`optional` and its comment says so, because that is exactly what an implementer
needs and cannot infer.

Where they differ, and why:

- Every capacity and allocatable number on a node, and on a lifecycle event.
  Absent means the size was not stated; zero means the node reported none of
  that resource. `NodeDevice.allocatable` is the sharpest of them: zero
  alongside a capacity of four is four accelerators, none schedulable.
- Every counter in `collection_coverage`. A counter's zero is a measurement.
- Every switch in `ConfigShape`, and `SourceHealth.synced` and `.failing`. A
  `false` is a stated posture, not an unfilled field.
- `NodeEvent.at_observed`. Today `false` and absent coincide, because an
  arrival's instant is exact. That is a coincidence between two meanings, and
  coincidences are what break later.

Where they do not differ, the field stays a plain scalar and the comment says
what the empty value means: `kind` and `source`, every identity or label string
whose empty value *is* its absence (`instance_type`, `zone`, a condition's
`reason`), `window_seconds` — a zero-length window is not a claim anyone can
make — and the two knobs whose zero is documented as "the default applies",
`profiling_top_n` and `spool_max_age_hours`.

**3. The corpus proves the schema.** `internal/pb/kubernetesv1` parses each
covered golden into its generated message with `protojson` and `DiscardUnknown`
**off**, so a field the corpus carries and the schema does not fails a test
rather than passing silently. A second test walks the golden JSON beside the
parsed message and fails on any key whose value did not survive as a *set*
field — which is what turns "declare presence field by field" from an intention
into a check. Removing `optional` from `intake_rejections.malformed` fails it.

**4. The JSON representation does not move.** Proto3's canonical JSON encodes
64-bit integers as strings and omits fields at their default value, so
`protojson` output is not the corpus: `"capacity_cpu_milli": 2000` gains quotes,
and every zero without presence disappears. The goldens stay hand-rolled until
the wire is binary. Two consequences of that decision are worth stating now:

- Integer widths are 64-bit throughout, following the Go declarations rather
  than the range each value happens to take. A mixed schema would render
  `capacity_cpu_milli` as a number and `capacity_memory_bytes` as a string,
  which is a difference between two fields that mean the same kind of thing.
- Timestamps are `google.protobuf.Timestamp`. They render as the RFC 3339
  strings already in the corpus, and being messages they carry presence for
  free.

**5. Protobuf is not on the wire in this change.** No payload byte moves, no
encoder changes, nothing is transmitted. What ships is the schema, proven
against the corpus, ready for a transport that comes later.

**6. Compatibility rules, stated because they are what the N-2 window rests on.**

- Field numbers are never reused. A removed field goes to `reserved`, number and
  name both. `buf breaking` against `main` is what enforces this; a comment
  asking people to remember is not.
- Enums begin at `UNSPECIFIED = 0`, and a reader tolerates an unrecognized value
  rather than rejecting it — the contract already says reason sets grow, so a
  reader that refuses an unknown value breaks §6 by construction. These three
  kinds declare no enum: their discriminators carry Kubernetes' own vocabulary
  (`"True"`, `"NoSchedule"`) or the agent's lower-case tokens (`"joined"`,
  `"structural"`), and canonical JSON renders an enum by its declared name,
  which is not those strings.
- The package is `rebuildstack.ingest.kubernetes.v1`. The observed system is in
  the package name because a kind name is unique within one registry and nowhere
  else.
- No monetary field anywhere: the protocol is denominated in resource units
  (ADR 0004).
- Nothing in the schema gives a response anywhere to carry configuration, a
  threshold, or a target. There are no service definitions and no response
  messages — only payloads the agent sends (ADR 0001).

**7. The files are public, and their comments say what a field is.** Its unit,
where the value comes from, and what its absence means as distinct from its
zero. Not what is measured or charged for, not what the backend does with it,
not what the schema is expected to grow into, and no pointer to a document the
reader cannot open: if a rule matters to an implementer, the comment states the
rule instead of citing where it was decided. This is the one place in the
repository where "cite the ADR by number" (ADR 0044) does not apply, because the
reader is outside it.

## Consequences

**Easier.** The distinction between a measured zero and an unreported field is
now carried by the schema rather than by a convention nobody wrote down, and it
is checked. A renumbered field or a reused name fails CI. Someone writing their
own collector, or auditing what leaves their cluster, has three payload kinds
described in a file they can read without this repository.

**Harder / given up.** Nineteen kinds still have no schema, and the corpus
remains their contract; a backend implementer has to know which of the two they
are reading. The generated Go is committed, so a schema edit is two files in one
diff and a stale regeneration is possible — `make proto` is the only supported
way to produce it. `optional` on a proto3 scalar is a synthetic `oneof`, so the
generated Go carries pointers for every counter; nothing reads those types yet,
and when something does, that is the cost of the distinction being real.

Declaring presence field by field is a judgment per field, and judgments
misjudge. The presence walk over the corpus catches only the fields whose golden
value is zero; a field wrongly left plain whose corpus value happens to be
non-zero passes. What that check does guarantee is that no zero the agent
currently ships is lost.

**Not changed.** No collection, no payload byte, no golden file, no RBAC, no
chart. `docs/security.md` is untouched because nothing about what is collected,
stored or transmitted moved. `backend-requirements.md` is untouched for the same
reason: its reader implements ingest against a wire, and the obligation to read
an absent counter as *unreported* rather than as zero lands with the transport
that carries it.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
