# 0022. The payload registry lives in code, and an amendment cannot stay silent

Date: 2026-08-22
Status: Accepted
Amends: 0003, 0009 §5, 0011 §7, 0012 §1, 0013 §4, 0021
Amended by: 0023, 0024, 0025

Amends the location of ADR 0012 §1's registry — its content and the provenance
classes of §2 stand; corrects three statements of fact in 0009, 0011 and 0021;
and answers the checkpoint that ADR 0003 introduced and no later decision ever
withdrew. The decisions of all five stand otherwise unchanged.

## Context

Three consecutive slices were planned from a premise that was no longer true,
and each time the error surfaced while writing the code rather than while
reading the document that should have prevented it.

- ADR 0021 states that object-level history "needs facts from objects other than
  pods, which is where the informer set actually grows". The ReplicaSet and Job
  informers have been running since the collector's first slice; only the
  handlers grow.
- ADR 0009 §5 says the scanner reports "only four aggregate counters". There
  have been five since ADR 0015 added `filtered_scope`.
- ADR 0011 §7 says GPL-2.0 eBPF code "lives in its own directory with its own
  `LICENSE`". Nothing is vendored; the bytecode is embedded in the upstream
  module, which is what the repository's `NOTICE` correctly says.

None of these is interesting on its own. What they have in common is: an
accepted ADR made a statement of fact about the artifact, the artifact moved,
and nothing anywhere could notice. This document is not written because three
sentences were wrong. It is written because there was no mechanism by which they
could have been right.

**The registry is the expensive case.** ADR 0012 §1 declared the payload kinds
"a fixed list, recorded here", and required a new kind to be added "by extending
this table in a superseding ADR, not by inventing a `kind` string at a call
site". The requirement was followed — every kind since has an ADR. The table was
not. ADR 0013 completed the provenance column, ADR 0017 added a kind, ADR 0019
renamed it, ADR 0020 and ADR 0021 added two more. A reader opening ADR 0012
today finds a table that is wrong in three rows and missing three of the ten
kinds that ship, with nothing in the file to suggest anything had happened to
it. The question "what does this agent send" had no answer anywhere — not in the
document that claimed to hold it, and not in the code, where it was spread
across ten call sites.

**The direction of the references is the mechanism that was missing.** Almost
every ADR already declared what it amended: 0007, 0008, 0010 and 0011 in
parentheses inside the `Status:` line, 0014 and 0016 through 0021 as a paragraph
below it. Two shapes, neither parseable, and two ADRs that amend others — 0013,
which closes 0012's recorded gap and relocates 0006 §3's "self-info", and 0015,
which extends 0009 §5 — declared nothing at all. But the deeper problem is that
all of it points backward. The reader who needs the warning is the one who opens
the *old* ADR, and the old ADR is the one file guaranteed to know nothing.

**Separately: `state.pb` was decided and then quietly abandoned.** ADR 0003 gave
the volume three things — `identity/`, `state.pb`, `spool/` — of which only the
spool exists. The checkpoint appears in no code, no manifest and no test; it
survives only in ADR 0003's title, in 0007's "the spool format … a checkpoint
file … is unchanged", and in `security.md` §6, which still describes contents
("watermarks of closed/acknowledged hours, the workload registry with metadata
content hashes, profiling rotation state") that have never existed.

It was abandoned for a good reason that nobody wrote down. ADR 0003 predates ADR
0006. When a window accumulated for an hour and shipped whole, losing it to a
restart was expensive. Minute-cadence snapshot delivery with supersede-by-key
ingest reduced that loss to the flush interval, and ADR 0007 then answered the
one remaining scenario — a restart during a backend outage — by making disk a
knob. Every later ADR reasoned as if the checkpoint were already gone: ADR 0017
prices the loss of the written-build marks at one redundant write, ADR 0020
accepts that "a closed window is written once and then dropped from memory".
The decision was reversed by consensus and never recorded.

## Decision

**1. The payload registry lives in `internal/sink/registry.go`, and the golden
bytes check it.** The table of kind, natural key, delivery discipline and
provenance moves out of ADR 0012 §1 into code, next to the writers. Two tests
compare it against the golden payloads in both directions: a kind that ships
without a row fails, and a row describing nothing that ships fails.

The reason it moves is not that code is tidier. It is that **a document cannot
fail.** ADR 0012's requirement — one natural key, one provenance, no kind
invented at a call site — is unchanged and is now enforced rather than asked
for. Each row names the ADRs that put it there, so the reasoning stays one hop
away; what the code owns is the list, not the argument.

Deliberately not in the registry: the spool filename each kind produces.
Filenames are asserted by the writers' own tests, and a field the registry test
cannot prove from the payload bytes is a field that will drift — which is the
failure this ADR exists to end, reintroduced one level down.

**2. An ADR's header is machine-readable metadata; its body stays immutable.**
The header carries `Amends: NNNN` and the mirror `Amended by: NNNN`, and a test
in `docs/adr` requires the graph to be symmetric, to point backwards, and to
name only ADRs that exist. `Status:` becomes a closed vocabulary — `Accepted` or
`Superseded by NNNN` — because free-form status text is prose pretending to be
metadata, and it is where four of these relationships had been hiding.

This narrows the immutability rule rather than breaking it. An accepted ADR's
Context, Decision and Consequences are still never edited: they record what was
decided when it was decided, and a change of mind produces a new file. What may
be maintained is the two header lines that say where the reader should go next —
information that did not exist when the file was written and cannot, by
construction, be written by its author.

The forward line is maintained by hand and checked by the test, not generated.
The failure message prints the exact line to paste. A generator would have made
the header a build artifact and invited the question of whether it was current;
a failing test cannot be out of date.

**3. `state.pb` is not built, and the volume holds only the spool.** Loss of
open collector state is bounded by the flush interval, and counter baselines are
re-established by the first-observation rule of ADR 0006 §2 and ADR 0020 §5 —
both of which already price this as accepted. `security.md` §6 stops describing
bookkeeping that does not exist.

This closes the decision rather than deleting it: if restart-boundary precision
is ever wanted, it is a new decision with its own reasoning, not a four-year-old
obligation nobody remembered taking on.

**4. Corrections of record.** ADR 0009 §5's "four aggregate counters" is five
since ADR 0015. ADR 0011 §7's GPL directory does not exist and should not: the
upstream module embeds the bytecode and `NOTICE` describes the combined work
accurately, which was the substance of that decision. ADR 0021's claim that
object-level history is "where the informer set actually grows" is wrong — the
ReplicaSet and Job informers already run; only the handlers grow. Five code
comments cite "ADR 0011 §5.5", which is a slice number in
`docs/plans/ebpf-node-profiling.md`, not a section of any ADR; they now cite the
ADR section they meant.

**5. Named open, not fixed here.** The audit that produced this ADR found five
things that need decisions of their own, and recording them is the point:

- **`ebpf_profile` declares `source: "ebpf"`**, while ADR 0012 §2 fixes the
  provenance vocabulary as structural / measured / journal / sampled and its
  registry names this kind `sampled`. ADR 0013 §4's claim that the registry is
  "fully realized in the bytes" is therefore not true of this kind. `ebpf` is a
  capture method rather than a class of claim: profiles pulled from a
  `/debug/pprof` endpoint would have to declare `pprof`, and the field would
  stop discriminating provenance at all. The registry records what ships and the
  row carries the discrepancy; changing the value is a protocol change with its
  own decision, and it is cheapest now, with ten goldens and no consumer.
- **ADR 0011 §3 places the profiling eligible set in the node's ConfigMap** and
  derives its safety bound from that. The set is enforced on the controller; the
  node cannot check it, because the reply is container IDs and the node has no
  way to resolve one to a namespace. The node does enforce every ceiling and the
  symbol allow-list. Either the reply grows the workload identity the node needs,
  or §3's bound is restated honestly — a decision, not a patch.
- **`security.md` promises a label selector** as a customer-facing filter. No
  ADR ever decided it and no configuration field exists. It is the only claim
  found that has no source at all.
- **`security.md` is written in the present tense about a finished product** —
  NetworkPolicy, the startup self-audit, the filter fingerprint, the identity
  Secret grant, mTLS transport and the `pprof` profile are all described as
  existing. The raw manifests in `deploy/` are honest that the chart is future
  work; the security document is not.
- **The controller is a `Deployment`**, while ADR 0008 derives "one writer, no
  races" from a single-replica StatefulSet. Harmless today and load-bearing by
  the time identity ships.

## Consequences

**Easier.** "What does this agent send" has an answer that cannot be stale,
because it fails CI when it is. An ADR that amends another cannot stay silent
about it, and the amended ADR cannot stay unaware — so planning a slice from an
old file now shows the reader, in the file itself, that the ground moved. The
five open items have a written home instead of living in whoever last noticed
them.

**Harder / given up.** Two header lines are now maintained per amending ADR, and
a test will refuse the pull request that forgets one. The registry has to be
edited alongside the writer and the golden, which is one more step in adding a
kind — deliberately, because the previous number of steps was zero and the list
drifted for nine slices. A test directory now exists under `docs/`, which is
unusual; the alternative was checking documents from somewhere that is not the
documents, and colocation is how the goldens already work.

**Deliberately not done.** The ADRs are not consolidated. Collapsing was
considered for the payload family — 0013, 0014 and 0017 through 0021 all amend
0012 — and rejected: they are not versions of one decision but seven separate
ones, each still in force, each carrying the alternative it rejected. Merging
them would produce a single unreadable file and discard exactly the reasoning
that keeps a settled question from being reopened. Once the registry lives in
code, nothing about them needs merging: none of them claims to hold the current
list.

The storage family — 0003, 0007, 0008 and the storage half of 0005 — is the
opposite case and is the one genuine consolidation candidate: one subject in
three timeframes, of which only the last is true. It is left for the slice that
builds identity, so that the consolidated record describes what shipped rather
than what is planned. Writing it now would reproduce the present-tense defect
named in §5 above.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
