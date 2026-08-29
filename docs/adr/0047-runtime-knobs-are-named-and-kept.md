# 0047. Five environment variables are named, kept and shipped; every other one is still dropped

Date: 2026-08-28

Status: Accepted

Amends: 0046 §2

Amended by: 0050

Narrows ADR 0046's blanket drop of `env` to a closed list of Go runtime knobs,
and ships them in `workload_metadata`. `envFrom`, `args` and `command` are
unchanged, and so is every variable not on the list.

## Context

ADR 0046 removed `env` at the informer transform, which was right for the reason
it gave: environment values are where connection strings and tokens get written
by hand. It removed something else along with them.

`GOMAXPROCS` left unset under a CPU limit is the most common waste in Go on
Kubernetes: the runtime counts the node's cores, not the cgroup's quota, so a
container limited to half a core starts as many scheduler threads as the machine
has CPUs and spends its quota being descheduled. `GOMEMLIMIT` unset under a
memory limit is the second: the collector has no idea a ceiling exists, so the
container is killed where the garbage collector would have run. Both are stated
entirely in the environment. Neither is visible anywhere else — the module list
can show that `go.uber.org/automaxprocs` is absent, which is evidence that the
default was *not* fixed, but not evidence about what was set instead.

Both are readable from a single pass over the cluster, so a report that names
them needs no profiling and no accumulated history.

There is a way to get this wrong that looks reasonable: keep `env` and let the
payload builder pick. That is the shape ADR 0046 exists to refuse — the values
would be held in the cache for the lifetime of the process and dropped at
serialization, which is the outcome CLAUDE.md invariant 4 ranks second.

## Decision

**1. A closed list of names, applied at the same transform.** `GOMAXPROCS`,
`GOGC`, `GOMEMLIMIT`, `GODEBUG`, `GOTRACEBACK`. Everything else is removed before
the object is cached, exactly as ADR 0046 has it. The list is in code, not in
configuration: a customer-editable list of variables to collect would be a
setting whose wrong value is a disclosure.

**2. A name is not a promise about the value's origin.** A variable called
`GOGC` whose value comes from a `secretKeyRef` or a `configMapKeyRef` is a read
of a Secret or a ConfigMap, which the agent does not do (invariant 4, and it
holds no RBAC for either). Those are dropped whatever their name.

**3. A knob read from the container's own limits is kept as the field path it
reads.** `valueFrom.resourceFieldRef` is the canonical way to set `GOMAXPROCS`
from a CPU limit, and it is the *good* configuration this finding is looking
for. The spec holds no value in that case — the kubelet resolves it at start —
so the payload carries `resource:limits.cpu` rather than a number. Naming the
source answers the question the finding asks: does this knob follow the limit?

**4. `envFrom` stays dropped entirely.** It imports a Secret or ConfigMap whole.
There is no name to filter on before the import happens, so there is no version
of this decision that applies to it.

**5. The values ship.** `workload_metadata` records carry `runtime_env`, absent
when nothing on the list is set — which is the state most of these findings are
about. Keeping a field in the cache that no payload carries would be collection
without a consumer, which is the thing this ADR was careful not to do.

## Consequences

**Easier.** The first-hour report can state the two findings above with the
configuration in hand rather than inferring it from a missing module. `GODEBUG`
and `GOTRACEBACK` come along because they are the same class of fact and cost
nothing extra: a fleet-wide view of which `GODEBUG` switches are set is
occasionally the whole explanation for a performance question.

**Harder / given up.** The promise in `docs/security.md` is no longer "no
environment variable is read". It is now a list, and a list is a thing a reader
must check rather than a thing they can take on faith. That is a real reduction
in how easy the guarantee is to verify, bought for a finding we believe is worth
it. The mitigation is that the list is short, is in one function, and is
enumerated in the customer document by name.

A value on this list is still operator-authored free text. `GODEBUG` in
particular is a comma-separated set of switches, and nothing stops someone
writing something else into it. The agent does not bound or truncate these
values, which is the same treatment the image reference already gets and is
called out for (ADR 0039).

**Not changed.** `args`, `command` and `envFrom` are dropped as before, and so is
every variable whose name is not one of the five. No new RBAC, no new watch, no
new privilege: this is a narrowing of a filter the agent already runs.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
