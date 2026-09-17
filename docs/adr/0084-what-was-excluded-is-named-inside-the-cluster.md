# 0084. What was excluded is named, inside the cluster and only there

Date: 2026-09-17

Status: Accepted

Amends: 0069 §1

Adds a second listener to the controller, bound to loopback, answering which
namespaces, pods and Jobs the filters admitted and which they refused, by name
and with the reason. No payload byte changes, no RBAC, no Service, no container
port, and no rule in the shipped NetworkPolicy.

## Context

The agent counts what it excludes and never names it. That is the promise
(CLAUDE.md invariant 6), and ADR 0054 built the coverage payload to keep it: how
many objects each of the customer's five controls removed, never which.

The promise is about what leaves the cluster. It has been read, everywhere it
has been implemented, as though it were about what the agent may *know* — and so
the customer, who owns every one of those annotations and can read them with
`kubectl`, learns from this agent only that nine of something were excluded.

Two questions follow from that and neither has an answer today.

**Someone turned a sensor off and nobody can say which.** An annotation is a
deliberate act by a person with access to an object. Later, a different person
reads a coverage number that moved. Finding the annotation means walking every
namespace, workload and pod by hand, because the object that carries it is
precisely the object nothing reports.

**"Why is my workload not in the report?"** has five possible answers — the
allow list, the deny list, three annotations at three levels — and the agent,
which has already computed the answer for that exact object, states only a
total.

`security.md` §11 promises that "filtering behavior is verifiable from output
rather than only from configuration". What that rests on today is a structured
log line and a set of counters, neither of which can be resolved to an object.

## Decision

**1. The filters answer by name, to a reader inside the cluster.** One line per
object the filter gates: the namespace, the pod, the Job, whether it is
collected, whether it may be profiled, and which control refused it. The pod's
blind spot is there too — a controller of a kind the agent cannot read
(ADR 0028 §3) — which is where a customer finds out that their workload
annotation never had a chance to work.

Nothing is retained to answer this. The informer caches already hold every
namespace, pod and Job whether admitted or not, so the listing is a projection
of live state through the same `Admit*` calls the event path makes, and the
answer cannot drift from the decision. Reading it counts nothing: the counters
describe decisions the agent acted on, and a read is not one (ADR 0054).

**2. Loopback, and any other host is a startup failure.**

Not a path on the health listener, which is where it would otherwise belong.
That port is opened by the shipped NetworkPolicy to **any** source, because the
caller is the kubelet arriving from the node's address and no pod selector in a
template can name it (ADR 0069 §5). A NetworkPolicy selects ports, not paths, so
a path there is reachable by every pod in the cluster — and the rule's own
justification is that every path on that listener "names nothing in the
cluster", which this one does not satisfy.

Two more reasons the policy cannot be the control: it is not rendered at all in
an installation with no node DaemonSet, and where it is rendered it is enforced
by the CNI, so on a cluster whose CNI ignores NetworkPolicy it is inert. The
bind address is the only control that holds in every case.

Loopback also answers authorization without inventing any. The only way in is
`kubectl port-forward`, which Kubernetes already gates on `create` of
`pods/portforward` in the agent's namespace — a permission the customer has
already granted to exactly the people who should be asking. Requiring a
projected token on a shared port would have added a credential, a failure mode
and a request to parse, to arrive at a narrower version of a permission that
already exists.

So `collection.listenAddress` accepts a loopback host or nothing, and refuses
anything else at startup rather than normalizing it. An address that exposed
this listing to every pod in the cluster must not be reachable by a typo.

**3. No query is parsed; the whole list is always answered.** Newline-delimited
JSON, one object per line, so a reader works through it as it arrives. A parameter would be a request this
endpoint acts on, which is the shape invariant 1 exists to refuse, and an
endpoint that filters can return an empty answer that a reader mistakes for
"nothing matched". Selecting is `jq`'s job. While the gating caches are still
filling the answer is a 503 and no names, because a short list here is
indistinguishable from a small cluster.

**4. It never becomes a payload.** This is the one surface that names what the
filters excluded, and what keeps those names inside the cluster is not the
handler — it is that nothing which can transmit knows the package exists. That
is a property of the import graph and is asserted as one.

## Consequences

**Easier.** The customer answers both questions themselves, in one command,
without sending anything to anyone: which object carries the annotation, and
under which control a given workload is missing from the report. The §11 promise
gains a mechanism that resolves to an object. And the support question that
needs a cluster we cannot see is now answerable by the person who has it.

**Harder / given up.** A second listener in the controller, and with it the end
of one-listener-per-role (ADR 0069 §1). The boundary is the property, not the
convenience: the health listener's paths answer without naming anything in the
cluster, and that property is what a rule opening it to the whole cluster rests
on. A path that names objects does not share that port.

Anyone who can port-forward into the agent's namespace reads the deny list,
which ADR 0054 §2 refused to *hash* into a payload. Inside the cluster that is
not the same disclosure — the reader can already list namespaces, and the list
is the customer's own configuration — but it is a real widening of who learns it
easily, and it is stated rather than left to be discovered (ADR 0039).

This covers the objects the controller's filters gate. The node's process scan
has exclusions of its own (ADR 0015) and is not answered here; it would be the
same shape on the node's listener, and is not built until someone asks.

Nothing here changes the inference ADR 0054 named: a workload that disappears
from the collected set as a counter moves can still be identified by
subtraction, by a backend that chooses to. This listing neither closes that nor
widens it — it is not shipped.

**Rejected.** Naming excluded objects in the coverage payload, behind a switch
the operator turns on. An opt-out annotation is an answer, not a question: a
customer who writes one has said what they want, and a tumbler that offers to
send the name treats their decision as ours to bargain with.

A `kubectl` plugin computing the same answer from the same filter code, with the
reader's own credentials. It answers a different question — what the filter
*would* decide, seen through the caller's RBAC — and the question worth
answering is what this agent decided through its own.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
