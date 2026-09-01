# 0063. A profile the collector dominates is still a profile

Date: 2026-08-31

Status: Accepted

Amends: 0011 §5, 0059

Changes one rule in `nodeprofile.Validate`: a profile must contain a service
frame, not rank one among its top five. No new collection, no payload change, no
privilege, no chart change.

## Context

The share of a workload's CPU spent in the garbage collector is a finding the
agent already has the data for. The symbol filter keeps `runtime.*` frames —
they carry no domain, so the rule that keeps the standard library keeps them
(ADR 0041) — and every shipped `ebpf_profile` and `pprof_profile` therefore
carries the collector's own frames beside the workload's. Computing the share is
arithmetic on bytes that already leave.

Except for the profiles where it matters most.

`Validate` ranked functions by cumulative sample value and required a service
function among the top five. Background marking runs on its own goroutines, so
those samples carry **no service frame at all**: their stacks are
`runtime.gcBgMarkWorker` → `runtime.gcDrain` → `runtime.scanobject` and so on.
A service spending most of its CPU collecting therefore fills all five top slots
with runtime frames, and its own code ranks below them.

So the profile that proves a collector problem was the one refused for being
about the collector. The stronger the evidence, the more certain the refusal —
and after ADR 0060 the refusal is counted, which makes the loss visible without
making it smaller.

## Decision

**1. The rule is presence, not rank.** A profile must contain at least one frame
that is the workload's own code — not `runtime.*`, not the `[filtered]`
placeholder — anywhere in any sample. A profile with none describes no workload
and is still refused.

The check walks the samples rather than the profile's function table, so the
answer does not depend on the table holding only what the samples reference.

**2. What the old rule was guarding is now guarded better.** ADR 0059 leaned on
this rank test as a mitigation: a service redacted by an allow-list that had
stopped matching would produce a profile of nothing but runtime and placeholders,
and refusing it turned a wrong answer into silence.

That mitigation is no longer load-bearing. ADR 0059 removed the cause — the node
reads the modules off the binary, so a default install no longer redacts the
service layer — and ADR 0060 made the symptom visible, because a rising
`third_party_dropped` beside a rising `profiles_invalid` is what an allow-list
that stopped matching looks like from outside the cluster. The remaining rule
still catches the case it was written for: a profile whose every non-runtime
frame was redacted has no service frame anywhere.

**3. The agent does not compute the share.** It ships the samples; which fraction
of them is the collector's is a division, and a payload that ships a division
cannot be re-divided by a consumer asking a different question (ADR 0062 §3,
ADR 0004's shape). What changes here is only that the profile arrives.

## Consequences

**Easier.** A workload burning its CPU in the collector produces a profile that
says so. That is a Tier-one answer — the fix is a `GOGC` or `GOMEMLIMIT` value,
not a code change — and it needed no new collection, only the removal of a rule
that refused the evidence.

Both capture paths gain it at once: the eBPF pipeline and the pprof puller share
this validator.

**Harder / given up.** The gate is weaker. A profile in which one service frame
appears once, among overwhelming runtime activity, now ships where it did not.
That is the intended case, and it is not distinguishable in the rule from a
profile that is nearly useless for any other question — so a consumer reading
one for a purpose other than "where did the CPU go" has to look at the shape
before trusting it.

**Not changed.** Nothing new is read, nothing new leaves, and no frame that was
redacted before is kept now. The filter is untouched; only the decision to ship
what it produced is.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
