# 0068. The agent yields first, and two objects it does not ship

Date: 2026-09-04
Status: Accepted

## Context

The chart says what the agent may read (ADR 0036, ADR 0037) and what it may not
reach (ADR 0009). It says nothing about what the agent costs the cluster it is a
guest of, and the four questions a customer's platform team asks at review were
all answered by omission:

```
priorityClassName              absent — the agent competes with the workloads it measures
PodDisruptionBudget            absent — no reason recorded either way
topologySpreadConstraints      absent — no reason recorded either way
terminationGracePeriodSeconds  absent — the shutdown flush pass runs inside a number nobody chose
```

An absence answers nothing. Two of these should stay absent and the reason has to
be written down; two should be present.

**The agent must lose the tie, not win it.** A pod with no `priorityClassName`
takes the cluster's `globalDefault`, or zero where there is none — the same
priority as every unclassified workload in the cluster. Under node pressure the
kubelet ranks by QoS class and then by priority, and the scheduler preempts by
priority; at a tie the agent is as likely to survive as the application it exists
to measure. That is the wrong way round for a measurement tool, and it is not a
matter of taste: an agent that survives the workload it displaced has changed the
thing it was installed to observe.

**A budget on one replica is not a safety net.** The controller is a single
replica by decision (ADR 0008, ADR 0026). `minAvailable: 1` on it forbids the
only eviction there is, so a node carrying the controller cannot be drained until
an operator deletes the budget — the agent becomes the reason a customer cannot
service their own cluster. `maxUnavailable: 1` on one replica forbids nothing and
is an object that exists to be seen.

**A spread constraint needs replicas to spread.** The controller has one; the
DaemonSet is already one pod per node by construction. There is nothing for the
constraint to say.

**The shutdown pass runs inside a budget the chart never named.** A SIGTERM
cancels the root context, `wg.Wait()` waits for the watchers and the periodic
flush goroutine, and then the shutdown pass writes the accumulating payloads
(`cmd/agent/main.go`). The one hard bound in that path is the node-intake
receiver's own 10s `Shutdown` budget; the watchers stop with their context, the
in-flight kubelet reads are cancelled with it and bounded at 10s regardless
(ADR 0045), and the pass itself is local writes into an emptyDir bounded at
512 MiB (ADR 0042). Measured ceiling: about ten seconds and a file write. What
the pass buys is up to one coverage interval — 60s — of `inventory`,
`container_restarts`, `pod_disruptions`, `job_runs`, `node_lifecycle` and
`collection_coverage`, which is exactly the minute a rolling upgrade or a drain
makes interesting. None of that was defended by a number.

**And a Go process in a container does not know its memory limit.** Since Go 1.25
the runtime reads the cgroup CPU bandwidth limit and sets `GOMAXPROCS` from it.
It does not read the memory limit. So the GC of a controller in a 256Mi container
targets a heap it chooses on its own and the container is OOM-killed — SIGKILL,
no shutdown pass, the 60s above lost, and a restart that reads to the customer as
a crashing agent.

`GOMEMLIMIT` fixes that, and the fraction is the whole of the decision: the
limit must sit far enough below the cgroup's that everything the Go runtime does
not account for still fits above it. Measured on the linux/amd64 build of this
binary:

```
.text        27.4 MiB
.rodata       9.9 MiB
.gopclntab   24.1 MiB   = 61.5 MiB of demand-paged, file-backed mapping
.noptrbss    32.1 MiB   anonymous, of which 32 MiB is a FIPS-only scratch buffer
                        untouched unless GODEBUG=fips140=on
```

Those pages are reclaimable and so do not by themselves cause an OOM kill, but
they sit in `memory.current` and `GOMEMLIMIT` is blind to every one of them.

The downward API cannot express a fraction. `resourceFieldRef` divides, rounds
up, and emits a bare integer: `divisor: 1Mi` against a 256Mi limit yields `256`,
which `GOMEMLIMIT` reads as 256 **bytes**. Only `divisor: 1` is safe, and that is
100% of the limit — the one setting Go's own documentation warns against. Helm
cannot compute the fraction either: it has no quantity parser, and the value it
would have to parse is whatever the operator wrote (`256Mi`, `1Gi`,
`268435456`, `0.25Gi`).

## Decision

**1. `priorityClassName` is a value on both roles, empty by default, and the
chart creates no PriorityClass.**

The chart accepts a name and refuses to invent a number. A priority value only
means something against the classes a cluster already has, so one chosen here
would be a guess about someone else's ordering; and a `create` switch beside the
name is the half-enabled component the install profile exists to prevent
(ADR 0036). `values.yaml` carries the four-line manifest of a negative-valued
class, because a negative value is what actually makes the agent yield in a
cluster with no `globalDefault` — every other pod there sits at zero, and a
non-negative class cannot get underneath it.

The chart refuses a `system-`-prefixed name outside `kube-system`. The
PodPriority admission plugin admits `system-cluster-critical` and
`system-node-critical` only for pods in `kube-system`, so such a name elsewhere
produces a DaemonSet that creates no pods and says nothing about why. A name that
does not resolve at all is the same failure and the chart cannot see it; that one
is disclosed, not caught.

**2. No PodDisruptionBudget ships, in any profile.** For the reason in Context:
on one replica the useful setting blocks node drain and the harmless one forbids
nothing. This is a decision, and `internal/chartrender/guardrail_test.go` fails
if a budget appears, so reversing it means arguing with a test rather than adding
a file.

**3. No `topologySpreadConstraints` ship, in any profile.** One replica has
nothing to spread and a DaemonSet is already spread. Same enforcement.

**4. `terminationGracePeriodSeconds` is a value: 30 on the controller, 10 on the
node, and the chart refuses a controller value below 15.**

30 covers the measured ~10s ceiling with room for the flush pass. It is also the
Kubernetes default, so it changes nothing today — its job is to be a number that
was chosen, and that a later change to the shutdown path has to be measured
against.

The node gets 10 because it has nothing to lose. It runs no shutdown pass and
holds no state that survives a restart: a lost report costs nothing and the next
pass re-scans (ADR 0010, ADR 0003). Its exit is bounded by one uninterruptible
scan pass — `nodescan.Scanner.Scan` takes no context — so the grace period is a
ceiling on how long the agent can hold up somebody else's node drain, and on a
DaemonSet that ceiling is worth having small.

The floor exists because the failure it prevents is silent. A controller grace
period below the receiver's own 10s `Shutdown` budget means SIGKILL lands before
the flush pass runs, and what is lost — the 60s of journals above — is lost
without a log line, a counter, or anything in the spool to notice it by. So the
value is an operator's to raise and not to quietly break: the chart states the
floor and refuses below it, the way it refuses an unknown profile.

**5. `GOMEMLIMIT` is 80% of the container's memory limit, and the agent applies
it.** The chart renders `MEMORY_LIMIT_BYTES` from `resourceFieldRef` on
`limits.memory`, and the agent multiplies by the fraction at startup and calls
`runtime/debug.SetMemoryLimit`. The fraction is a constant in `cmd/agent`, not a
value: a knob nobody can set correctly is the inert knob ADR 0025 abolished.

80% because the reserve has to hold what the measurements above name. At the
chart's 256Mi default that is 51 MiB against 61.5 MiB of mappable text; 90%
would leave 26 MiB, which is thinner than this binary's resident text is likely
to be, and a `GOMEMLIMIT` the process exceeds through memory the runtime cannot
see is a `GOMEMLIMIT` that did not convert anything.

The env var is rendered only where a memory limit is rendered with it. With no
limit set the downward API substitutes the node's allocatable memory, and the
agent would be told it may use 80% of the node — a worse answer than no answer,
arrived at silently.

`GOMAXPROCS` is deliberately not set: the toolchain this module builds with
(`go 1.26`) already reads the CPU limit. The chart supplies the one the runtime
does not read.

## Consequences

**Easier.** The four questions above have answers in one file, and two of them
are answers rather than absences. An OOM kill of the controller becomes GC
pressure — visible in the agent's own memory profile, recoverable, and it keeps
the shutdown pass — where before it was a SIGKILL that took a minute of journals
with it. A drain of a node carrying the agent is bounded by a number the chart
states.

**Harder / given up.** The promise that the agent yields first is only true for
an operator who applies a negative-valued PriorityClass and names it. We ship the
manifest and cannot ship the object; an installation that sets nothing keeps
today's behaviour, which is a tie. And an operator who names a *higher* class
inverts the promise entirely — the chart cannot read a PriorityClass's value at
render time, so this is disclosed rather than enforced.

The 80% fraction is a floor on wasted headroom at large limits: an operator who
raises the controller to 1Gi reserves 205 MiB that a smaller reserve would have
given to the heap. This is accepted — `GOMEMLIMIT` is a ceiling the GC reacts to,
not a target it fills, so the cost only exists in a cluster large enough to
approach it.

Two roles now carry a grace period an operator can lower. The floor covers the
controller's silent case; the node has no floor beyond a positive value, because
it has no loss to protect.

**Not changed.** Nothing about what the agent collects, stores or transmits.
`priorityClassName` and `terminationGracePeriodSeconds` are already collected
fields of the placement block (ADR 0031), so setting them on the agent's own pods
adds no field, no source and no grant — `priorityclasses` and
`poddisruptionbudgets` are read already (ADR 0032), and no PriorityClass object
is created for the agent to read. No payload byte moves and
`internal/sink/registry.go` is untouched, so `docs/security.md` states no new
promise and gains no line.

One honest side effect: the two pods gain an environment variable. It is named
`MEMORY_LIMIT_BYTES` and not `GOMEMLIMIT` — the value is the raw limit, which the
Go runtime would read as the limit itself — so it is not among the five names
ADR 0047 keeps, and the agent's own `workload_metadata` does not carry it.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
