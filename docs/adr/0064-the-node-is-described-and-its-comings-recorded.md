# 0064. The node is described, and its comings and goings recorded

Date: 2026-09-01

Status: Accepted

Amends: 0012

Widens `node_metadata` from ten fields to a description of the machine, and adds
`node_lifecycle`, the first payload that says what the fleet *was* rather than
what it is. No new RBAC — the node informer already lists and watches whole node
objects — and no new privilege.

## Context

`node_metadata` was drawn for one job: turning a node name into a zone and an
instance type so a usage record could be attributed. It carried the two sizes,
four labels and two fields of `status.nodeInfo`, and the type's comment said
"nothing else is read from node objects" as though that were a promise rather
than a consequence of nobody having needed more.

Four questions arrive at that payload and leave unanswered.

- **Why a pod will not schedule.** CPU and memory are two of at least five
  scheduling dimensions. A node can be idle in both and still refuse every pod
  because it is at its kubelet pod ceiling, out of ephemeral storage, tainted,
  or `Ready=False`. Every one of those is on the object the agent already holds
  in its informer cache.
- **What the expensive machines are.** An accelerator node costs an order of
  magnitude more than the general-purpose node beside it, and to this payload the
  two are the same row with different labels. A fleet's GPUs are invisible.
- **What software the fleet runs.** `kernelVersion` is collected because it
  decides whether the eBPF profiler can attach. The kubelet version, OS image and
  container runtime sit in the same struct, are read in the same call, and are
  what an upgrade plan is written against.
- **What the fleet used to be.** Every payload the agent ships about nodes is a
  snapshot that replaces its predecessor. A node that leaves is simply absent
  from the next one. Fleet churn — the central fact about a cluster running spot
  capacity or an aggressive autoscaler — is reconstructible only by diffing
  consecutive snapshots, and only at flush granularity, and only if none was
  missed.

The last of these is different in kind from the first three. The others are
fields on an existing payload; this one is a fact no snapshot can hold.

## Decision

### 1. `node_metadata` describes the machine

Age (`metadata.creationTimestamp`), the three remaining `status.nodeInfo`
strings, and four more resource dimensions: ephemeral storage and pods, both
capacity and allocatable.

Pods and ephemeral storage are core resources and were left out for no reason
better than that the first finding did not need them. They join as named fields
like CPU and memory.

### 2. Three lists, each bounded by an allow-list

The parts of a node object with unbounded shape do not enter as they are. Each
follows the discipline ADR 0019 set for build settings and ADR 0031 for
placement: an allow-list rather than a deny-list, a length bound, a drop counted
rather than a value truncated.

**Conditions** are allow-listed **by type** — the four the kubelet maintains plus
`NetworkUnavailable`. Kept: status, reason, last transition. Never kept:
`message`. It is free text, and the two things that write it — the kubelet and
node-problem-detector — put paths, device names and command output there. The
type allow-list is what makes `reason` safe to carry: a custom condition's reason
is written by whoever installed the agent that sets it, and no custom type
survives the list.

The transition time is kept and the heartbeat is not. The heartbeat moves every
few seconds on every node in the cluster; carrying it would make every node
differ from its previous view on every flush, which is both noise in the payload
and a defeat of the change-detection the informer callback rests on.

All five allow-listed conditions are reported whenever the node reports them,
healthy or not. A list holding only problems would make "this node is fine" and
"the agent did not read this" the same bytes, which is exactly what ADR 0048 §2
forbade for the update strategy.

**Taints** are kept field for field — key, value, effect — with the same 128-byte
bound as placement values. This is the one place operator-written strings enter
from the node side, and it is justified by symmetry: tolerations are *already*
collected from every pod spec, and a toleration without the taint it answers is
half a fact. It says what a pod would put up with, not what the fleet fences off.

Unlike tolerations, no taint is filtered as a cluster default. The two the node
controller adds — `not-ready` and `unreachable` — appear only on a node that is
actually broken, so there they are signal rather than the noise the matching
tolerations are on every pod.

**Extended resources** are allow-listed by **hardware-vendor prefix**, not by
exact name and not wholesale. Prefix because a vendor's line is open — a GPU and
a MIG slice of it are the same fact under different names — and an allow-list
because the rest of that namespace belongs to whoever runs the cluster, where a
resource named for an internal team, project or licence is an identity that must
not travel. A vendor absent from the list is a one-line change; an operator's
private resource name leaking is not recoverable.

### 3. `node_lifecycle`: a journal, because a snapshot cannot hold this

A windowed journal keyed by (window start, window length), superseding, exactly
the shape ADR 0021 settled for disruptions. The window is a delivery boundary
rather than an aggregation, and it bounds the spool's file count by time rather
than letting a churning autoscaler control it.

**A record carries the node's size, kind and accelerators.** This duplicates
`node_metadata` for a live node and is the entire point for a dead one: once the
object is deleted the snapshot has no row to join against, so without the size on
the event, "twelve nodes left this hour" can never become "and they were 96 cores
of spot capacity, four of them with GPUs".

**An arrival is only reported when it can be proved.** The test is the node
object's creation timestamp against the moment this process started watching, not
the fact that the informer had not seen the node before: at cache sync the add
handler fires for every node in the cluster, and counting those would report an
agent restart as the entire fleet arriving at once. This is ADR 0034's rule —
what predates observation is not an event — applied to nodes. The cost is a node
created in the seconds before the agent started, which is reported as no arrival
at all rather than as a false one.

**A departure carries the agent's observation instant, and says so.** A deleted
object is simply absent; there is no deletion time to read. The record's `at`
therefore means two different things depending on the event, and rather than let
one field silently mix provenances, `at_observed` marks the case where it is the
agent's clock. The precedent is `restarts_before_observation` and
`reasons_unobserved`: state the limit in the payload rather than in a document
the reader does not have.

The journal is in memory and is lost on restart, like the other three. A
departure that happens while the agent is down is not reported — it appears only
as a node missing from the next snapshot, which is exactly where the fleet stood
before this ADR.

### 4. What the reductions refuse is counted

A `nodes` block in `collection_coverage`: conditions refused for their type,
devices for their vendor, taints for an oversized string, and version strings
that did not fit. Aggregate counts, never a name — CLAUDE.md invariant 6, and the
same treatment `placement` already gets. A cluster whose nodes do not fit these
bounds is visible rather than quietly under-described, and the device counter in
particular is how the vendor allow-list learns it is short a vendor.

## Consequences

The node payload roughly triples in size: five conditions of about sixty bytes
each are the bulk of it, and on a 500-node fleet that is on the order of 150 KB
per flush. It is a superseding snapshot, so it does not accumulate.

`NodeInfo` is no longer comparable, and the informer's "report only when the view
changed" test moved from `==` to a reflective deep equality. That is affordable
because node objects change on the order of minutes — since Kubernetes 1.17 the
kubelet heartbeats to a Lease, not to the node — and the one sub-second field it
does write is the condition heartbeat, which §2 deliberately does not collect.

Taints are the first customer-written strings the agent reads from a node.
`team=payments:NoSchedule` names an internal team, and that string now leaves the
cluster. The justification is symmetry with tolerations rather than a claim that
the string is harmless: the same value was already leaving in every pod spec that
tolerates it, and refusing it here while carrying it there would have been a
promise the payloads do not keep.

What is still not collected: node labels beyond the four read by name,
annotations, addresses, `status.images`, `status.volumesInUse`, and condition
messages. `nodes/proxy` is unchanged; nothing here reaches a kubelet.
