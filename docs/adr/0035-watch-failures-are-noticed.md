# 0035. A cache that stopped being fed is noticed: gating caches stop the agent, the rest degrade their payload

Date: 2026-08-25

Status: Accepted
Amends: 0033 §5

Closes the limitation ADR 0033 recorded as known and unfixed. No new kind, no new
field, no new RBAC, no change to any payload's shape: `unavailable_sources`
already exists and now answers a second question with the same word.

## Context

ADR 0033 chose `HasSynced` as the availability signal and got the start of an
agent's life right: an empty list and a refused read became different states,
and a customer who declines one of the seven policy grants keeps the usage
collection they installed the agent for.

`HasSynced` is a one-way latch. It turns true when the first LIST succeeds and
never turns back. A cache whose watch is refused afterwards therefore keeps
reporting availability while the store answers every query from what it last
held, and the reflector retries in the background forever. ADR 0033 §5 recorded
this, judged the damage to be staleness, and declined to build the detector.

Two things about that judgement were wrong.

**Stale is not the right word for a superseding snapshot.** `workload_policy` and
`cluster_policy` replace their predecessor and state what is true at
`captured_at`. A budget deleted after the refusal keeps shipping; one created
after it never ships; and the payload declares every source read, because the
omission of `unavailable_sources` is itself the claim that nothing was missed.
The report is not behind — it is wrong, and it is wrong identically at every
capture until someone restarts the agent. §5's own mitigation, "any restart
reports correctly", rests on an event this project deliberately made rare: the
controller is a single-replica Deployment with no volume and can run for weeks.

**The latch is not a property of the seven policy caches.** It is a property of
every informer. The pod index, the owner chain and the namespace cache are what
every other payload is assembled from, and a refusal there is not a degraded
payload but a blind agent producing payloads of entirely ordinary shape. The
comment above `PodWatcher.Run` had already stated the intended behaviour —
*a failing watcher must bring the agent down rather than leave it half-blind* —
and the agent could not honour it, because nothing told it the watch had died.

There is a third case the same blindness produces, and it is the worst of them.
`WaitForCacheSync` has no timeout. An agent installed without the `pods` grant
never syncs, never errors and never exits: a Running pod, a healthy Deployment,
no data, and nothing anywhere that says why.

## Decision

**1. Watch failures are recorded, and the recording is per cache.**
`SetWatchErrorHandler` fires on every failed `ListAndWatch`, including a failed
LIST, and is the only signal client-go offers — there is no matching callback for
success. Each informer gets a health record holding when its current run of
failures began and when it was last seen failing.

**2. The cause is not examined.** ADR 0033 §2 decided that the agent does not
classify why a source is unreadable, and the same holds here. A revoked grant, a
deleted ServiceAccount whose token stops being accepted, an API version dropped
by a cluster upgrade and an authorization webhook that changed its mind are one
state — the cache is not being fed — and the payload has no field in which the
difference could be expressed.

What classification would have bought, persistence buys instead: a transient
error happens once, a refusal repeats until someone fixes it.

**3. Recovery is inferred from silence, with the reflector's own number.** A run
of failures ends after two minutes without one. That is not a guess: while a
watch is failing the reflector retries at most every [30,60) seconds, and
client-go states the identical judgement in `defaultBackoffReset` — "if we don't
backoff for 2min, assume API server is healthy". Adopting its constant keeps the
two from disagreeing.

A streak's length is measured between its first and last failure, never up to
now. One failure spans nothing and is therefore never an outage, which is the
intended reading of a single dropped connection.

**4. A gating cache that fails for five minutes stops the agent.** Gating is the
split ADR 0033 §1 already drew — a cache that gates a signal against one that
adds one — applied to the running agent rather than only to its start. Pods,
replicasets, jobs, deployments, statefulsets, daemonsets, cronjobs, namespaces
and nodes gate; the seven policy caches do not.

Not the first error, because a restart is not free: the in-memory windows, the
restart baselines and the spool are lost with the process (ADR 0026), so dying
on a five-second API server hiccup would cost more than the blindness it
prevents. Five minutes is several retries, and it bounds how long the agent can
be blind before it says so.

The watchdog runs from before the first cache sync, so the never-synced case and
the went-dark case end the same way: an exit whose message names the resource,
and a pod in CrashLoopBackOff where a customer can see it. The informers run on
the watchdog's context rather than the caller's, or the factory's shutdown would
wait for watches that stop only when the process is signalled.

**5. A policy cache that failed within the last three minutes declares itself
unavailable.** The existing field, unchanged, now covers both halves of one
statement: a source is unavailable if it never filled *or* if it is failing now.
The two are not distinguished, for the same reason the causes are not — a
consumer does the same thing with either.

Three minutes is longer than the [30,60)-second retry interval, so a live
refusal is declared at every capture rather than at some of them. A permission
restored is therefore declared unavailable for a capture or two more, and a
transient error can cost one capture's completeness claim. Both are the safe
direction: the field overstating what was missed is a smaller error than the
payload overstating what was read.

**6. What the payload carries is the source name.** The error text is logged and
goes no further. A refusal names the agent's own ServiceAccount and a resource
class rather than any customer object, but the rule that identities never leave
the cluster (CLAUDE.md invariant 6) is not one to defend case by case.

**7. Known limitation: revocation is noticed when the watch is next
re-established.** An established watch is not re-authorized by the API server, so
a grant removed from a running agent has no effect until client-go re-opens the
connection — which it does on its own timeout, between five and ten minutes.
Detection therefore lags a revocation by up to ten minutes, and the gating exit
by up to fifteen.

This is not fixable from inside the agent without polling permissions, which
would mean creating SelfSubjectAccessReviews: a write verb in the audit log of a
product whose promise is that it only reads, answering a narrower question than
the handler already answers. The lag is recorded here so it stays a decision.

## Consequences

**Easier.** The three ways an agent could go quietly blind — never synced,
stopped being fed, and the wait with no timeout — now end in either a named exit
or a payload that says what it could not see. A customer who narrows the
ClusterRole of a running agent gets the same honest answer they already got when
narrowing it before install.

**Harder / given up.** The agent can now stop for a reason that is not its own
fault, and a sustained API server outage will restart it after five minutes,
losing an interval of accumulated windows. That is the trade ADR 0026 already
implies: state that cannot survive a restart is state whose loss is preferable to
its silent corruption.

`unavailable_sources` also became slightly less precise: it can name a source
that is readable again but failed within the window. The alternative — a field
that is precise and occasionally false — is worse for a consumer that must decide
what a report rests on.

**Not changed.** No payload shape, no RBAC, no kind, no collection behaviour when
the read succeeds. The seven policy caches still degrade rather than stop, which
is ADR 0033's decision and the bug it fixed.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
