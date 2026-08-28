# 0046. The informer cache is the source, so the fields the agent promises not to collect are removed there

Date: 2026-08-27

Status: Accepted

Amends: 0031 §7

Amended by: 0047, 0048

Moves the drop of `env`, `args` and `command` from the payload seam to the
informer's transform, so the values are never held rather than never shipped.
No payload changes.

## Context

CLAUDE.md invariant 4 orders the outcomes: not collected beats collected and
dropped, which beats collected and not sent. `docs/security.md` §8 states the
promise as "`env`, `args` and `command` are discarded before anything is stored
or transported", and ADR 0031 §7 backs it with a test on the encoded payload
bytes.

The payload was never the first place the fields existed. The controller built
its factory as `informers.NewSharedInformerFactory(clientset, 0)` — no
transform, no field selector — so every Pod, ReplicaSet, Deployment,
StatefulSet, DaemonSet, Job and CronJob in the cluster sat in memory as the API
server returned it, for the lifetime of the process. Nothing read those fields.
They were simply kept, which is the middle outcome the invariant ranks second.

Three things ride along with them:

- `kubectl.kubernetes.io/last-applied-configuration` is a verbatim copy of the
  object as applied. Removing `env` from a container while leaving this
  annotation moves the values rather than dropping them.
- `managedFields` is read by nothing here, and is larger than it looks:
  `kubectl get -o json` has hidden it since 1.21, so the objects an informer
  actually receives are not the objects an operator inspects.
- A field selector cannot help. The pod field selectors the API server supports
  are `spec.nodeName`, `status.phase` and the name and namespace — none of the
  fields at issue.

Measured on kind (Kubernetes 1.36.1, 9 pods), reading `/api/v1/pods` rather
than through `kubectl`, so the bytes are the ones a watch delivers:

    all pods, as received            77,463 B
    without managedFields            46,917 B
    without env / args / command     73,180 B
    without both                     42,634 B

45% of what the pod cache held was material nothing reads, against a container
limit of 256Mi.

## Decision

**1. One transform, registered on the factory, applies to every informer.**
`informers.WithTransform` runs before an object is stored, which for a watch-fed
cache is the earliest point the agent controls. That is what "filter early"
means here; the payload test of ADR 0031 §7 stays where it is, now as the second
of two checks rather than the only one.

**2. It removes, from every container list of every pod spec an object
carries** — a Pod's own containers, init containers and ephemeral containers,
and the template inside a ReplicaSet, Deployment, StatefulSet, DaemonSet, Job or
CronJob — `env`, `envFrom`, `args` and `command`.

**3. It removes `managedFields` and the applied-configuration annotation from
every object, whatever its kind**, including the ones with no pod spec at all.
Both are unread, and a kind added to the factory later gets this much without
anyone remembering to ask for it.

**4. The number of cached objects is not reduced, only their size.** Scoping the
informers to the configured namespaces would cut more, and is not done here: the
namespace filter supports a deny-list as well as an allow-list, and the
annotation opt-outs are properties of objects the agent would then no longer be
watching. A watch topology that depends on the filter's shape is a bigger
decision than this one.

## Consequences

**Easier.** The promise in `docs/security.md` §8 is now true of the process and
not only of the payload: a heap dump of the controller does not contain a
customer's environment variables, and neither does a core file, a debugger, or
anything else that reads its memory. The pod cache is 45% smaller on the
measurement above.

**Harder / given up.** Every cached object is now a *modified* copy of what the
API server holds, so a future feature that needs one of these fields cannot read
it from the lister and will find the absence at runtime rather than in review.
That is the intended direction — a field this agent must not collect should be
inconvenient to reach — but it is a real edge, and the transform is the one
place to look when a field is unexpectedly empty.

The transform is a type switch, so a workload kind added to the factory without
a case keeps its template's `env`. The test enumerates the kinds and fails on
one that is not stripped; enumeration is what a test can do here, and it does
not know about a kind nobody added to it.

**Not changed.** No payload, no golden file, no RBAC grant, no chart value, and
no field that was collected before is missing from anything shipped. The two
paths that read pod specs — the payload builders and the profile join — read
fields this transform does not touch.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
