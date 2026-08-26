# 0040. The channel authenticates a node, not just the node role

Date: 2026-08-26

Status: Accepted

Amends: 0010 §2, 0015 §1, 0039 §4

Gives the node→controller channel a per-node identity, taken from the token
rather than from the request, and ships the NetworkPolicy three earlier records
described as already shipping. No change to what is collected, to any payload,
or to any RBAC rule.

## Context

ADR 0010 built the channel on a projected ServiceAccount token, verified locally
against the cluster JWKS, with the subject pinned to the node role's
ServiceAccount. That establishes that a caller is *the node role*. It does not
establish *which node*, because every pod of a DaemonSet presents the same
ServiceAccount and therefore the same subject.

The node name arrived somewhere else: in the request body, where the caller
chose it. `ScopeHandler` and `TargetsHandler` discarded the verified identity
outright — `if _, err := h.verifier.Verify(…)` — and answered on `req.Node`. The
inventory and profile handlers kept the identity for a log line and joined on
`report.PodUID` / `report.ContainerID`, which `LookupContainer` checked for
internal consistency and not for whose node the pod was on.

So one node's token was, for that token's lifetime:

- a cluster-wide read of the controller's admitted-pod index — `POST
  /v1/node-scope {"node": "<any other node>"}` answers with the pod UIDs the
  filters admitted there, and node names are predictable on every managed
  platform;
- a way to file Go-inventory facts and captured profiles against workloads on
  other nodes, with the forged payload landing in the spool as a genuine record
  for that namespace, workload and digest.

ADR 0010 §Consequences priced the compromise as "POST fabricated Go-inventory
facts for this one cluster". That was an understatement in two directions: the
more sensitive payload is the profile, and the reads were not priced at all.

Two smaller findings sit on the same boundary. `nodeIntake.expectedSubject`
defaulted to empty, meaning "accept any subject that satisfies the audience" —
and the audience is not a secret, because the kubelet mints a token for whatever
audience a pod's projection asks for, with no RBAC on `serviceaccounts/token`.
The chart has always set it, so no install was exposed; the code's default was
the dangerous one and a test named it correct behaviour. And the NetworkPolicy
that ADR 0010, ADR 0011 and ADR 0015 all cite as a compensating control did not
exist (ADR 0039 §4 recorded that and deferred building it to here, on the
grounds that reachability and identity are one boundary).

What makes the identity fix cheap is that Kubernetes has been carrying the
answer all along. A token projected into a pod is bound to that pod, and the
kubelet writes the pod's node into the token's private claim block. Measured on
Kubernetes 1.36.1, decoding a token projected with a non-default audience:

```json
"kubernetes.io": {
  "namespace": "…",
  "node": {"name": "runtime-agent-e2e-control-plane", "uid": "3799b633-…"},
  "pod": {"name": "…", "uid": "…"},
  "serviceaccount": {"name": "…", "uid": "…"}
}
```

The verifier parsed `jwt.RegisteredClaims` and discarded the rest.

## Decision

**1. The node identity comes from the token.** `nodeauth.Identity` gains `Node`
and `NodeUID`, decoded from the `kubernetes.io` node claim. Nothing else in the
system may establish which node is calling.

**2. A token with no node claim does not verify.** Such a token was not
projected into a running pod — it was minted directly, through TokenRequest with
no bound object — and its bearer has no node at all, so there is nothing to
compare a request against. Accepting it and then trusting the body would restore
exactly the hole being closed. The floor this assumes is the baseline the README
already states.

**3. Every endpoint compares the node it is asked about with the node the token
names, and refuses a mismatch with 403.** 403 rather than 401 on purpose: the
token was valid and the caller is who it claims to be. What it may not do is
speak for somebody else.

The node field stays in the request body rather than being removed. Removing it
would change the wire format between two components that upgrade independently —
the DaemonSet and the Deployment roll separately — so an upgrade would put one
side in front of a shape the other does not know. Requiring the two to agree is
pure tightening: a node speaking for itself matches, in either version order.

**4. The join is node-scoped too, not only the request.** `LookupContainer`
becomes `LookupContainerOnNode` and resolves only pods on the reporting node,
with the node taken from the verified token. This is redundant with decision 3
today and deliberately so: decision 3 lives in four handlers and this lives in
the one place every fact is attributed, so a future endpoint that forgets the
check still cannot misattribute.

**5. `nodeIntake.expectedSubject` is required.** The controller refuses to start
with node intake enabled and no subject, naming why. The two neighbouring fields
keep their defaults because an unset listen address or audience degrades safely;
this one degraded to an open door.

**6. The NetworkPolicy ships, restricting the receiver to the node DaemonSet.**
Ingress only, one rule, one peer, the receiver's port. The controller's egress
is left alone: it talks to the API server today and to a backend later, and a
policy written now would be rewritten by the change that gives it something to
egress to.

Two limits are written into the template rather than assumed away. A
NetworkPolicy is enforced by the CNI, so on a cluster whose CNI does not
implement policies the object is inert — it is not a boundary the API server
keeps. And it restricts reachability, not identity: everything in decisions 1–4
is what actually decides what a caller may say.

The first of those is worse than a limitation, it is a silent one, and the chart
now says so at install time in a `NOTES.txt`. Measured on this repository's kind
cluster: a policy admitting only `role: friend` was accepted by the API server,
and a pod labelled `role: stranger` reached the target anyway, exit 0. There is
no error, no warning and no status — an unenforced policy is indistinguishable
from an enforced one by inspection. An operator who is told the receiver is
restricted deserves to be told that verifying it is their step, so the notes name
the check and name which CNIs enforce.

Detecting it from the agent was considered and rejected: the controller holds no
grant on `networkpolicies`, and widening RBAC to produce a warning would trade a
real permission for a cosmetic one — against ADR 0009's rule that nothing is
requested just in case.

**7. The identity is proven against a real cluster, and the policy is not.** A
new e2e speaks to the receiver from a sidecar in the node pod using the node's
*own* mounted token — the property is about what the kubelet writes into a
token, which no unit test can establish — and asserts the refusal on all four
endpoints, with the caller's own node as the positive control. The unbound-token
case is asserted the same way.

The policy gets no such test, because kind's default CNI does not implement
NetworkPolicy. The e2e creates it, which proves the API server accepts what the
chart renders and that the channel still works with it present, and the chart
test asserts its shape. That is the whole of the evidence and the ADR says so
rather than letting a green suite imply more.

## Consequences

**Easier.** A compromised node is now bounded by the node it compromised. The
controller's admitted-pod index stops being readable cluster-wide from any one
node, and a forged fact or profile can only concern workloads that are actually
there — which is the difference between a bad record and a bad record about
somebody else's workload. The receiver is no longer reachable from every pod in
the cluster on clusters whose CNI enforces policies.

**Harder / given up.** The channel now depends on a claim the platform supplies.
A Kubernetes distribution that omits the node claim from projected tokens would
make every node fail to authenticate — visibly, at the first request, with a
message naming the claim, and not silently. That is the right failure direction
and it is a real new dependency.

A node whose name changes under it — deleted and recreated during a capture
window — has its in-flight reports refused until the kubelet rotates the token.
`NodeUID` is decoded and not yet compared; a node recreated under its old name
is the case that would need it, and adding a check nothing has needed would be
the kind of unmeasured tightening this pair of ADRs exists to avoid.

What is still not closed, and is not this slice: the token remains a bearer
credential travelling in cleartext in-cluster, so a party who can sniff pod
traffic can still replay one — now bounded to the node it was sniffed from. And
`jti` is present in every token and unused, which is where replay tracking would
start if the cleartext transport is ever kept rather than replaced.

**Not changed.** No payload, no collection, no RBAC rule, no filter. The node
sends what it sent before and the controller writes what it wrote before.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
