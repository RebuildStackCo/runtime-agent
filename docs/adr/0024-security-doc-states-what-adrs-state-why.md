# 0024. `security.md` states what is true; the decision records state why

Date: 2026-08-22
Status: Accepted
Amends: 0020 §2, 0022 §5
Amended by: 0044

Closes two of the five items ADR 0022 §5 recorded as open — the label selector
and the document's present tense — and gives `security.md` the mechanism that
ADR 0022 gave the ADRs. Three items remain open there.

## Context

`security.md` is the promise to customers about what the agent can access and
what leaves the cluster. It had grown to 750 lines, and roughly a third of it
was a second copy of the decision records: §10.1 restated ADR 0016 including its
argument about recording rules and downsampling; §10.2 restated ADR 0015's
Decision; the on-node scanning subsection restated ADR 0009, 0015, 0017, 0018
and 0019 across 89 lines; the metadata, journal and observation subsections
restated ADR 0012, 0014, 0020, 0021 and 0013.

That was not laziness. CLAUDE.md requires `security.md` to be updated in the
same pull request as any change to what the agent collects, and what each slice
did with that rule was append its ADR's summary. The rule is right; the habit
built a duplicate.

**The cost is measurable, and it is the same cost ADR 0022 addressed.** A fact
kept in two places is maintained in one. The sentence "four aggregate counters"
was stale in ADR 0009 §5 *and* in `security.md` §8 — one wrong claim, two files.
The eligible-set claim that ADR 0022 §5 recorded as false existed in three
copies: ADR 0011 §3, `security.md` §7.2, and the network table of §5. Fixing the
code comments in one pull request left two of them standing.

Three further defects were found, all of the kind a mechanism catches and a
reader does not.

**The section that claims to enumerate what leaves the cluster did not.** Three
of the ten payload kinds — `usage_snapshot`, `usage_window`, `ebpf_profile` —
were never named anywhere in the document. The two usage payloads are the
highest-volume things the agent produces and appeared only as "hourly rollup
histograms". A reviewer could not map the document onto the wire.

**A cross-reference pointed at a section that did not exist.** §8 sent the
reader to "§11, your controls"; §11 was titled *Agent self-reporting* and listed
no controls. The controls were spread across §4, §11's opt-out subsection, and
one that does not exist at all.

**The first design principle was contradicted by its own document, twice.** §1
opened with "the agent holds no write verbs on any Kubernetes resource"; §4
disclosed `get`/`update` on the identity Secret, and §9 repeated the categorical
claim a third time. ADR 0008 had said explicitly that the categorical version
must go, and §4 was updated while §1 and §9 were not.

## Decision

**1. `security.md` states what is true; the ADRs state why.** Where the document
argued a decision, it now asserts the outcome and links the record. The
enumerations stay — what is collected, what leaves, what privileges are held,
what the controls are — because those are what a review needs and they exist
nowhere else in readable form. The reasoning does not, because it exists
already.

**2. A claim that is not built is marked `[planned]` where it is claimed.** Not
in a footnote and not in a status table at the top — at the sentence, so a
reviewer reading §4's RBAC table or §6's identity model cannot mistake intent
for property. The marker covers the backend transport and mTLS, the identity
Secret grant, the enrollment lifecycle, the `pprof` installation profile and its
pod probing, the shipped NetworkPolicy, the startup self-audit, and the filter
fingerprint payload.

This is the honest form of a document that describes a designed system while
the system is being built. The alternative — deleting every unbuilt claim —
would leave a security review unable to see where the product is going, which is
information a reviewer legitimately wants.

**3. There is no label-selector filter, and this is a decision rather than an
omission.** `security.md` listed one among the customer's controls, in two
places, and ADR 0020 §2 repeated it. No ADR ever decided it and no configuration
field ever existed — the only claim the audit found with no source at all. The
controls are the namespace allow/deny lists and the opt-out annotation on a
namespace or a pod; profiling adds its own empty-by-default eligible set. If
label-based filtering is ever wanted it is a new decision with a config field, a
fourth exclusion reason and an end-to-end test, not a sentence.

**4. The parts of the document that are lists are checked by tests**
(`docs/security_test.go`). The payload table must be exactly the registry of
ADR 0022 — a kind that ships and is not listed is an undisclosed payload, and a
kind listed that nothing ships is a capability the agent does not have. Every
internal anchor must resolve to a heading, and every link into `adr/` must
resolve to a file.

What is deliberately *not* checked is whether the prose is true. No test reads
"the agent never reads Secrets" and confirms it. What a test can hold is the
part that is a list, and the list is where the drift was.

## Consequences

**Easier.** A reviewer can map the document onto the wire, and the mapping
cannot silently go wrong. The reasoning has one home, so correcting a decision
no longer means remembering a second file. The `[planned]` markers make the
gap between the designed system and the shipped one a property of the document
rather than something a reader has to discover.

**Harder / given up.** Two conventions to maintain. Nothing enforces that a
`[planned]` marker is removed when the thing ships — that is a real gap, left
open deliberately rather than solved with a mechanism nobody would trust; the
markers are few and each belongs to a slice that will touch this file anyway.

**The honest result on size: the document did not get shorter.** About 130 lines
of duplicated reasoning came out and about the same number of enumeration,
tables and precision went in — the payload table that was missing, the controls
table that was scattered, and the eligible-set paragraph that was wrong. What
changed is what the length is made of, not how much of it there is. Recording
this rather than claiming a reduction is the point of the document this ADR is
about.

**Not changed.** The long subsections that are enumerations — node privileges,
field minimization, the build-settings allow-list, on-node scanning — stay long.
They are the answer to the question a security review is actually asking, and
compressing them would trade the document's purpose for its page count.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
