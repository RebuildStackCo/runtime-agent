# 0054. Coverage says how much was hidden, never what

Date: 2026-08-30

Status: Accepted

Amended by: 0057, 0058

Amends: 0012 §2, 0033

Adds the payload kind `collection_coverage` and a fifth provenance class,
`agent`. The counters it carries already existed; none of them left the cluster.

## Context

"Nothing found" is a result, and it is the result a customer most needs to trust.
An empty report and a broken agent produce identical bytes today: the backend
receives some payloads and has no way to say "we looked at 412 pods, 37 of them
were excluded by your own namespace filter, and one cache we hold a grant for
never synced".

Everything needed to say that is already computed. The filter keeps twelve
counters, the placement reduction keeps two, the node scanner keeps five, the
inventory store keeps nine, and the watch health of every non-gating source is
tracked continuously (ADR 0033, ADR 0035). All of it goes to a structured log
line and stops there. `security.md` §11 promises that "filtering behavior is
verifiable from output rather than only from configuration" — and the output is
the agent's own log, which travels the customer's log pipeline and reaches
nobody who renders a report.

The node scanner's counters are worse off still: they cross the channel inside
`nodescan.Report`, arrive at the controller, and are dropped on the floor by a
join that reads only the binaries beside them.

## Decision

**1. A fifth provenance class, `agent`.** The four classes of ADR 0012 —
`structural`, `measured`, `journal`, `sampled` — all answer "where in the
cluster did this come from". This payload's subject is the agent itself. Calling
it `measured` would be the discriminator lying in the one payload whose entire
purpose is to be trusted about its own limits, so the vocabulary widens by one
and `registry_test.go` records the widening.

**2. No hash of the configuration. Counts and a time instead.** The backend
needs to tell "the data changed" from "the filters changed". The obvious
mechanism — a digest of the effective configuration — is wrong here, for a
reason specific to what that file holds.

The configuration is a handful of short namespace names. A digest of a
low-entropy value is reversible by trying the plausible ones, and the list it
would expose is the **deny list** — precisely the set the customer chose to hide
from us. The allow list is no secret (those namespaces are named in every other
payload); the denied one is the whole of the privacy question, and hashing it
would ship it in a form that merely looks safe.

So the payload carries the configuration's *shape*: how many entries each list
holds, which switches are on. Nothing to reverse, because nothing is concealed —
these are numbers we are willing to state plainly. And it carries `config.since`,
the modification time of the file, which answers "have the filters changed since
the last upload" better than an opaque token does. A time is not content.

The file cannot change under a running agent — the chart stamps a checksum of the
ConfigMap onto the Deployment, so an edit replaces the pod — which is why this
needs no watch and no reload path.

**3. Effective access is measured, not declared.** The payload reports, per
non-gating source, whether its cache filled and whether it is failing. This is
better evidence than the `SelfSubjectRulesReview` the earlier plan called for: a
grant the ClusterRole holds but an admission webhook or a network policy defeats
reads as *granted* in any review of the rules, and as *failing* here. A report
resting on a source needs to know which of those is true.

It also avoids a question that deserved its own decision rather than a quiet
slip: `SelfSubjectRulesReview` is a `create` call, made by an agent whose
customer document says it only ever gets, lists and watches. That call is not
made, and if it is ever wanted it arrives with its own ADR.

The gating caches — pods, namespaces, nodes, the owner chain — are absent from
the list on purpose. Their failure stops the agent (ADR 0035), so a payload
arriving at all is the evidence about them.

**4. Node scan counters are summed, and the sum is of latest passes.** Aggregate
rather than per node: "how many processes did the fleet skip" is a cluster
question, and a per-node breakdown would grow the payload with the fleet to
answer it in a shape nobody asks. Latest pass per node rather than a running
total, because the counters describe one walk of a process table — adding
successive walks would count one long-lived process once per scan interval.

**5. It ships on every flush, including one with nothing to say.** This is the
only kind for which "nothing changed" is not a reason to stay silent: its
staleness is a fact about the agent where silence in every other kind is a fact
about the cluster, or about nothing. An old `captured_at` says the agent
stopped.

## Consequences

**Easier.** A report can state what it rests on and what it did not see. "Nothing
found" becomes a sentence with evidence behind it rather than an absence
indistinguishable from a failure. Two `[planned]` markers leave `security.md`,
and `backend-requirements.md` — which has described this payload as arriving
since it was written — stops disagreeing with the code.

**Harder / given up.** One inference channel is not closed by any of this, and
it is honest to name it rather than let a reviewer find it (ADR 0039).

The backend sees every collected workload. If one disappears from the collected
set at the moment `excluded_workload_annotation` moves from 0 to 1, which
workload was hidden follows by subtraction. The counter does not create that
channel — the disappearance is observable without it — but it does explain it.
The alternative is silence, under which the backend concludes the workload was
deleted and tells the customer something false. Explaining an observable change
is the better of the two, and the customer's protection against the inference is
that the object was already collected before they opted it out, not that the
count is secret.

Nothing here closes it: a count fine-grained enough to be useful is
fine-grained enough to subtract. What the design does guarantee is that no name
of an excluded namespace, workload or pod appears in this payload or any other,
and `TestCoverageNamesNothingItExcluded` asserts it against a configuration
whose deny list holds real names.

**Not changed.** No RBAC, no new read, no new API call — every counter in this
payload was already being computed and thrown away. The `create` verb still
appears nowhere.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
