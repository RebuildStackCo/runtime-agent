# 0023. A profile's key names the build; its provenance names the claim

Date: 2026-08-22
Status: Accepted
Amends: 0011 §6, 0022 §5

Amends ADR 0011 §6 — the profile payload's natural key loses `source` and its
spool filename gains the image digest — and closes two of the five items ADR
0022 §5 recorded as open. Nothing else in either decision changes.

## Context

Two defects in one payload, found by the audit behind ADR 0022. They look
unrelated and are the same question asked twice: what identifies a profile, and
what kind of fact is it.

**The provenance field carried a capture method.** ADR 0011 §6 defined the
profile's key as "(namespace, workload, container, image digest, time window,
`source=ebpf`)" — `source` there meant *how the samples were taken*. ADR 0012 §2
then made `source` the provenance discriminator, with a closed four-class
vocabulary — `structural`, `measured`, `journal`, `sampled` — and its registry
named this kind `sampled`. Two different concepts landed on one field name, and
the older meaning stayed in the bytes.

Everything else had already moved. `docs/backend-requirements.md` documents the
four classes and names `sampled` among them; ADR 0012's registry says `sampled`;
ADR 0013 §4 declared the registry "fully realized in the bytes", which was true
of every kind but this one. The payload was the last holdout, and ADR 0022
recorded it rather than quietly fixing it.

Left alone it gets worse rather than staler. `ebpf` is not a class of claim, so
a second profile source would have to declare its own value — and at that point
`source` no longer discriminates provenance at all, which is the single job ADR
0012 created it for.

**The filename dropped half the key.** `WriteProfile` named its files
`profile-<namespace>-<workload>-<container>-<start>-<end>.json`. The image
digest was in the payload and not in the name, so it did not separate anything.

That is a silent overwrite, not a theoretical one. The node cuts every window on
one pair of boundaries and ships one report per targeted container
(`cmd/agent/pipeline.go`), so two replicas of a workload's container on the same
node produce two reports with identical namespace, workload, container and
capture interval. The controller joins each to a build and writes both under the
same name; the second replaces the first, with no counter and no log line. The
rollout case — two replicas of two builds — is the exact situation the digest
was put in the key for, and it was the one being lost.

The code said otherwise. `WriteProfile`'s comment read "concurrent or rotated
captures of the same workload never overwrite one another", and
`TestProfilesDoNotSupersede` varied only the capture window, so it asserted the
claim in the one case where it holds trivially. This is the shape of defect ADR
0018 exists to end: complete-looking output that is quietly truncated.

## Decision

**1. `ebpf_profile` declares `source: sampled`.** The field classifies the
claim — a profile is an estimate from a fraction of a window's instants, with
the failure modes of statistics — and says nothing about the instrument. Which
capture produced it is what the payload kind already names.

**No `capture` field is added.** It would be speculative today, since there is
one profile source, and unnecessary tomorrow, since a second one arrives as its
own kind: a `/debug/pprof` profile has no container-ID join and different window
semantics, so it cannot share this kind's natural key anyway. A field
duplicating what `kind` states is a field that will eventually disagree with it.

**2. The spool filename carries the whole natural key**, image digest included,
as `profile-<namespace>-<workload>-<container>-<digest>-<start>-<end>.json`. The
digest segment is twelve hex characters after the algorithm separator — the
conventional short form — and every non-hex character is discarded, so nothing a
registry could place in a digest string can introduce a path separator.

This is truncation, and ADR 0019 refused to truncate. The rules do not conflict
because they govern different things: ADR 0019 was about values that leave the
cluster, where a prefix of an unexpected string ships looking legitimate. Here
the full digest is in the payload and the shortened form is a local file name.
Two builds colliding on twelve hex characters is not a risk worth pricing.

**3. A capture with no digest is named `nodigest`**, not given an empty segment.
For this payload the state is close to impossible — sampling a container's CPU
means it was running, so its digest existed — and when it happens it means the
controller's informer had not caught up, never that the container had not
started. The literal makes the state visible in a directory listing instead of
leaving a gap that reads like a bug, and it cannot collide with a real digest.

**4. Two of ADR 0022 §5's open items are closed** — the provenance value and,
for this payload, the natural key. The other three stand: the profiling eligible
set enforced on the controller rather than the node, the label selector
`security.md` promises and no decision ever made, and `security.md`'s present
tense about unbuilt things.

## Consequences

**Easier.** The registry, the backend contract and the bytes now say the same
word, and ADR 0013 §4's claim is true of every kind without exception. Two
replicas' captures of one window both survive, and so do two builds' captures
during a rollout — which is when a profile is most worth having, because it is
when a regression is being introduced.

**Harder / given up.** One golden payload changed by one field; the other nine
are byte-identical, which is the evidence that the change is confined. Spool
filenames changed shape, so anything reading the spool by name gains a segment.
The full-path capture e2e reads the spool listing and matched on the prefix
through the container name, so it needed no change — recorded because it is luck
rather than design, and a stricter matcher would have broken.

**Known gap, carried forward rather than fixed.** ADR 0020 recorded that
`oom_kill` filenames embed unescaped Kubernetes names and that their combined
length can exceed the 255-byte filename limit. Profile filenames have always had
the same property, and this decision adds thirteen characters to them. Neither
is a traversal risk — Kubernetes names cannot contain path separators, and the
digest segment is filtered to hex — and neither is fixed here. A naming scheme
that bounds length is one decision for both kinds, not two.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
