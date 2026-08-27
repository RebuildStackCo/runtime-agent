# 0044. One fact in one place, and the places have ceilings

Date: 2026-08-27

Status: Accepted

Amends: 0024 §1

Extends ADR 0024's rule from `security.md` to code comments, and gives it the
mechanism it never had: `docs/prose_test.go` fails on a comment run longer than
eight lines and on a `security.md` that grows past its ceiling.

## Context

ADR 0024 found `security.md` at 750 lines with roughly a third of it restating
the decision records, cut it, and stated the rule: this document says what is
true, the ADRs say why. It shipped with `docs/security_test.go`, which checks the
payload table against the registry and resolves every cross-reference.

Five days later the document was **1139 lines**. Everything added in between was
written the way ADR 0024 had just finished removing.

The measurement, taken 2026-08-27 across the whole repository:

    docs/security.md      1139 lines, §8 alone 398
    docs/adr/             44 records, 5327 lines, mean 121, max 204
    Go                    28303 lines, 5328 of them comment (18%)
                          10 files between 37% and 48% comment
    117 comment runs longer than 8 lines

`internal/nodeintake/nodematch.go` was the shape of it: 26 comment lines above 16
lines of code, and nearly all of the 26 was ADR 0040's Context and Decision
rewritten in the second person.

**The rule was right and the failure was mechanical.** ADR 0024's own argument
applies to itself: a fact kept in two places is maintained in one. The tests it
shipped hold the parts that are lists, and a list was where the drift was *then*.
Length is not a list, so nothing failed while the document doubled.

**Code comments were never covered.** ADR 0024 is about one document. The same
habit ran through the Go tree, where an ADR reference and a rewritten ADR look
identical in review and only one of them stays true.

## Decision

**1. Three places hold prose, and each answers one question.** An ADR says *why*
a decision was made — alternatives, threat models, measurements, what was given
up. `security.md` says *what the customer is promised*. A code comment says what
a reader of those lines cannot see in them, and cites the ADR by number rather
than retelling it.

This is ADR 0024 §1 with the third tier added. The test for a comment is whether
deleting the ADR would make it wrong: if the comment can only be maintained by
maintaining the ADR too, it is a copy.

**2. A comment run over eight lines is a defect.** Enforced across every Go file
in the repository, tests included — a test file is read more often than the code
it covers, not less. Eight is where the measurement put the boundary between
comments that orient a reader and comments that argue a case; the 117 runs above
it were, with few exceptions, arguments that already existed in an ADR.

What survives the cut is what a comment is for: the measurement nobody would
reproduce (`CapEff: 0000000000201000` for `drop: [ALL]` beside
`add: [SYS_ADMIN]`), the invariant that is not visible locally, the pointer.

**3. `security.md` has a ceiling, and it is a ratchet.** 920 lines, no section
over 290. Raising either is a decision that appears in a diff and has to be
argued for, which is the whole difference between this and a convention.

The numbers are where the document landed once the restated ADRs were removed,
not a target chosen first. They are **not** ADR 0024's 750, and pretending
otherwise would be the mistake this ADR exists to stop: five payload kinds have
been added since — `job_runs` (0029), `deployment_revisions` (0030),
`workload_policy` and `cluster_policy` (0032), `restart_counters` (0034) — and
each is a row in the disclosure table plus a paragraph explaining it. §8 is the
section a security review reads to learn what leaves the cluster; it is long
because the answer is long.

**4. ADRs are not capped.** They are where the length is supposed to go. A cap
here would push argument back into the code and the customer document, which is
the failure this ADR is fixing.

## Consequences

**Easier.** `security.md` is 913 lines rather than 1139, and what left it was
duplication rather than disclosure — every payload kind, every field-minimization
rule and every honest caveat is still stated. A reader who wants the reasoning
follows one link.

**Harder / given up.** Some reasoning is now one link away rather than in front
of the reader, and a comment that says "ADR 0041" is worth less to someone
reading the file offline. That is the trade ADR 0024 already accepted for
`security.md`, extended.

The eight-line rule is a proxy, and proxies misjudge cases. A genuinely dense
eight lines can be worse than a clear twelve. The rule is still worth having:
it is checkable, and the failure it prevents — an ADR quietly forked into a
comment — is one review does not catch.

**Not changed.** No agent behaviour, no payload, no golden file, no RBAC, no
collection. Every edit in the implementing pull request is a comment, a document,
or a test.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
