# 0048. Four first-hour findings name fields the agent did not collect

Date: 2026-08-28

Status: Accepted

Amends: 0017 §2, 0032 §1–2, 0046 §2

Adds probe schedules and the workload's update strategy to `workload_metadata`,
Services to `workload_policy`, and a version to each module in `go_build`. Adds
one resource to the ClusterRole (`services`). Extends the informer transform of
ADR 0046 to the probe handlers, which were being cached whole.

*Corrected 2026-08-28, the day it was accepted and before any release: §2 first
named the field `rollout` and gave unread kinds an `unread` marker. The first
run against a real cluster refuted both — see §2.*

## Context

Every decision here so far has answered the same question one fact at a time:
*is this fact worth collecting?* That question has no stopping rule. Asked the
other way — *what sentence do we want to be able to say, and what does it need?*
— it terminates, and it produces a different list.

Five findings are meant to be true from a single read of a cluster, with no
window closed and no profile captured: what a node drain takes down, what is
already burning, what is throttled against its own limit, one number for the
whole cluster, and Go hygiene. Checking each against what the agent ships found
four fields missing, and none of them was missing by decision:

- **Nothing says a liveness probe exists.** A liveness probe is the only part of
  a spec that restarts a *healthy* container on a schedule, and the two failure
  modes — one more aggressive than the readiness probe beside it, or identical to
  it and therefore not an independent check — are visible in the numbers alone.
- **Nothing says how a workload replaces its own replicas.** `workload_policy`
  reports the budget that bounds an eviction; no eviction is involved when a
  Deployment rolls itself, and `maxUnavailable: 100%` with `minReadySeconds: 0`
  takes a workload down without a budget being consulted.
- **Nothing says traffic is routed to a workload.** One replica is a batch size
  or an outage depending on whether anything sends it requests, and the agent
  reads only the pod's declared `containerPorts` — an announcement, not a route.
- **`go_build` carries module paths without versions**, by ADR 0017 §2. So "one
  library at four versions across the fleet" cannot be said, and neither can any
  finding that compares a dependency version against another.

Reading the probe fields turned up something the third item did not predict.
ADR 0046 removes `env`, `envFrom`, `args` and `command` at the transform, on the
grounds that a container's free text must not be *held*, not merely not shipped.
A probe's `exec` handler is a command, and its `httpGet` handler carries
`httpHeaders` — where an `Authorization` value is written by hand. Both have been
sitting in the controller's cache since the first informer, and so have the
`preStop` and `postStart` hooks, which nothing reads at all.

## Decision

**1. Probe schedules are collected; probe handlers are emptied at the
transform.** `workload_metadata` records carry, per container, the liveness,
readiness and startup probes as `initialDelaySeconds`, `periodSeconds`,
`timeoutSeconds`, `failureThreshold`, `successThreshold` and the handler's
*kind* — `exec`, `httpGet`, `tcpSocket`, `grpc`. Everything the handler names is
cleared in `dropUncollectedFields` before the object enters the cache: the exec
command, the HTTP path, host and headers, the TCP host, the gRPC service. The
lifecycle hooks are removed whole.

The kind is kept and the target is not, because the finding asks whether two
probes are the same check, not what either calls. The numbers are the API
server's defaulted ones, which are the numbers the kubelet will use.

**2. The workload's update strategy ships as `workload.update_strategy`.** Type,
`maxUnavailable`, `maxSurge`, a StatefulSet's `partition`, and
`minReadySeconds`, read from the Deployment, StatefulSet or DaemonSet the pods
belong to. Those three kinds and no others.

**The field is not called `rollout`, and that is a decision rather than a
preference.** `Rollout` is the kind name of the Argo custom resource, which this
agent already reports in `workload_kind`. One record carrying
`workload_kind: Rollout` beside a field called `rollout` puts two meanings of
one word in one object, and the generic English noun is the one that loses.
`updateStrategy` is Kubernetes' own field name on StatefulSet and DaemonSet, so
it is borrowed rather than invented.

**Absence means the agent did not read a strategy, and claims nothing beyond
that.** The first version of this decision tried to say more: kinds known to
replace no replicas carried nothing, and everything else carried
`rollout: {unread: true}` so that a Job could be told from an Argo Rollout. That
was wrong twice over, and one run against a real cluster showed both. It needs a
complete list of owner kinds that do not update themselves, which nobody has —
the list omitted `Node`, so every static control-plane pod on a self-managed
cluster was reported as having an update strategy the agent had failed to read.
And the claim itself is a judgement: whether an unknown custom resource updates
its replicas is not something the agent observes, and this agent reduces rather
than concludes (ADR 0004).

What distinguishes the cases is already in the record. `workload_kind` ships in
the key, the kinds this agent reads are three and are named in
`docs/backend-requirements.md`, and what any other kind implies is a rendering
question for the side that renders.

It is a fact of the workload, and the record's key is narrower than that, so it
is nested and not flattened (ADR 0014) — a `workload` object beside the existing
`pod` one. `maxUnavailable` and `maxSurge` are kept as written, because a count
and a percentage are not interchangeable when the replica count moves; that is
the treatment a budget's own declaration already gets.

It is read when the payload is built and not when a pod was described. Editing
`maxUnavailable` rolls nothing, so a value captured at describe time would stay
wrong until something unrelated happened to the pods.

**3. `go_build` modules carry versions, which makes it a dependency inventory.**
This reverses ADR 0017 §2 and is worth stating as the reversal it is. That ADR
declined versions on the grounds that a path with a version is a
vulnerability-scanning feed and that adopting one is "a new decision, not a field
added quietly". This is that decision, made for one reason: the fleet-wide
findings that motivate the whole profiling arc — one internal module hot in many
services, a dependency's cost measured version against version — are arithmetic
over versions and cannot be approximated without them.

What is given up is real. `security.md` said `go_build` "carries module paths
only, so it is not and cannot be used as a vulnerability-scanning feed", and that
sentence is now false. The payload is a software bill of materials for every Go
build in the cluster, from which anyone holding it can derive known
vulnerabilities. Two things bound it rather than excuse it: the payload is
already scoped to admitted workloads and already carries the main module, the Go
version and the build's VCS revision, so this widens a disclosure rather than
opening one; and the agent still ships no vulnerability finding of its own —
reachability is the gate here (ADR 0038), and a module graph is not one.

A dependency redirected by a `replace` directive is reported under the path and
version the build *required*, flagged `replaced`. The replacement's own path is
not read: a local replacement's path is a directory on the build machine, which
is the class of string the build-settings allow-list exists to keep out
(ADR 0019).

**4. Services are added to `workload_policy`, and `services` to the
ClusterRole.** Name, type, headless, and the internal and external traffic
policies. The selector is resolved through admitted pods, exactly as a budget's
is, so a Service pointing only at excluded pods attaches to nothing and is never
named. A Service with no selector — an ExternalName, or one whose EndpointSlices
are written by hand — attaches to nothing either.

The cache is a degrading source (ADR 0033): withholding the grant costs the
`services` list and names itself in `unavailable_sources`.

Addresses are not read. A ClusterIP is cluster-internal and a load balancer's is
the customer's public address, and no finding here needs either; `clusterIP` is
read only for the one comparison against `None`. Ports are not read: the
container's own declared ports already ship, and the mapping between them is a
question no first-hour finding asks. `trafficDistribution` and EndpointSlices are
not read — they answer a zone-routing question that belongs to a later finding,
and collecting ahead of one is the habit this ADR exists to break.

## Consequences

**Easier.** Each of the five first-hour findings can now name a payload field for
every clause it states. The probe change also closes a hole ADR 0046 was written
to close and missed: a heap dump of the controller no longer contains a
customer's probe commands or the bearer token in a probe header.

**Harder / given up.** The `go_build` promise is weaker and the document has to
say so plainly, which §3 does. The payload also roughly doubles: ADR 0017
measured 200–1500 modules for a typical service, and a version string is about
as long as the path it follows. `go_build` is write-once per image digest, so
this is one larger file per distinct build rather than a per-flush cost, and the
spool's bound (ADR 0042) is what it lands against.

`security.md`'s file ceiling moves 920 → 930 (ADR 0044). It is paid for by three
things the document did not owe before — a grant, a class of collected field, and
the disclosure in §10.4 — and the section ceiling does not move: §8 came out of
this change four lines smaller than it went in, because the room was found by
deleting ADR rationale that had drifted back into it.

The ClusterRole gains a resource, so the "first widening" of ADR 0032 is now the
second. `services` is one of the least sensitive objects in the API — it holds no
customer data beyond names and routing — but the grant is cluster-wide in a
cluster-wide installation, like every other row.

No CRD is read, and the closed ClusterRole that §9 of `docs/security.md` leans
on gains no vendor resource. Reading Argo Rollouts properly is a larger decision
than a field — a first custom-resource grant, an informer for a kind that may not
exist in the cluster, and a strategy whose canary and blue-green shapes are not
this struct — so it is left to its own ADR. Until then the rollout dimension of a
first-hour report is blank for an Argo-managed workload, and blank is what the
payload says.

**Not changed.** No new payload kind, no registry row, no change to any natural
key, no verb that writes, no new privilege on the node, and nothing new is read
from a pod's `status`. Probe handlers, lifecycle hooks, `env`, `envFrom`, `args`
and `command` all remain uncollected — the list of what the transform removes got
longer, not shorter.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
