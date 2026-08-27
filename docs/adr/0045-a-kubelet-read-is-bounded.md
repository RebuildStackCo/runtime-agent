# 0045. A kubelet read is bounded in time and in size, and one node's silence costs one node

Date: 2026-08-27

Status: Accepted

Amends: 0006 §1

Gives the two kubelet reads a deadline and a size limit, reads the nodes
concurrently, and sets the API client's rate limit explicitly. No change to what
is collected, to any payload, or to any grant.

## Context

Usage collection has exactly two call sites, both `GET` through the API server
proxy: `/stats/summary` and `/metrics/cadvisor` (ADR 0006 §1). Neither carried a
deadline, neither carried a size limit, and the nodes were read one after
another in the goroutine that also runs the snapshot flush.

**Nothing else supplied the bound.** client-go's transport bounds the dial and
the TLS handshake, not the response. `rest.Config.Timeout` is not the instrument
either: it sets `http.Client.Timeout`, which would also cut the informers'
watches. The API server proxies whatever the kubelet sends for as long as it
keeps sending, so a kubelet that accepts the connection and then goes quiet held
the poll indefinitely — and with the reads serialized, it held every node behind
it and the snapshot flush with them.

Measured on the kind cluster (Kubernetes 1.36.1, one node), by adding 40 pause
pods to a 9-pod node:

    pods    /stats/summary   /metrics/cadvisor
       9        44,367 B           687,903 B
      49       167,654 B         3,076,138 B

That is ~3.1 KB and ~60 KB per pod. At the kubelet's default ceiling of 110 pods
the extrapolation is ~355 KB and ~6.7 MB — the exposition is the larger read by
an order of magnitude, and it is the one that grows with container count.

**A plain limit is not a bound but a silent truncation.** Measured:
`io.LimitReader` cutting the cAdvisor exposition at a metric-family boundary
makes the decoder return the families it got and `nil` — a node with fewer
containers, which is indistinguishable from a quiet one. Same for a truncated
summary that happens to close its JSON.

**Client-side throttling was at its default**, which `rest.Config` documents as
5 QPS with a burst of 10 when the fields are left zero. One usage cycle is two
requests per node, so a 75-node cluster already needs more than the limiter
allows in its 30-second interval. The limiter waits inside the request's own
context, so a deadline added on top of it would have fired on our own throttling
as readily as on a slow kubelet.

## Decision

**1. Every kubelet read has a deadline of 10 seconds.** Per read, not per node
and not per cycle: the two paths fail independently and are already counted
independently (ADR 0013).

**2. The reads are size-limited, and passing the limit is an error.** 8 MiB for
the summary, 64 MiB for the exposition. They differ because the reads differ:
the summary is unmarshalled from a buffer, and up to `kubeletFetchConcurrency`
of them exist at once, so its limit is a heap bound — 8 MiB is ~23× the
extrapolation above and, at eight in flight, a quarter of the controller's
256Mi. The exposition is decoded family by family off the wire and costs
bandwidth rather than heap, so its limit can be ~10× the extrapolation and still
be nothing.

Exceeding either is a failed read, never a short one. A truncated response
decodes as a valid smaller response, which is the failure this limit exists to
prevent rather than to cause.

**3. Nodes are read concurrently, eight at a time; only the reading.** Ingestion
stays in the polling goroutine, in the order the nodes were listed. The
accumulator and the per-container baselines keep a single owner and no mutex,
which is what makes the delta rules of ADR 0006 §2 readable at all.

**4. The API client's rate limit is stated: 50 QPS, burst 100.** Chosen from the
need — two requests per node per 30 seconds is 50 QPS at ~250 nodes, informer
traffic aside — not from what the API server can bear, which is the API server's
own question and is what Priority and Fairness is for. Stating it also stops the
limiter from consuming the deadline that decision 1 just added.

## Consequences

**Easier.** One silent kubelet costs its own two deadlines and nothing else: the
other nodes are read in the same cycle, and the failure reaches the payload as
`polls_failed` rather than as an absence (ADR 0013). A response too large to
trust is a counted failure with a reason in the log, not a node that quietly
appears to be running fewer containers.

**Harder / given up.** A cluster whose kubelets are *all* unreachable still
overruns the cycle: with eight workers it takes `ceil(N/8) × 20 s`, and the
snapshot flush shares that goroutine, so snapshots are late for as long as it
lasts. A cycle-wide budget would fix it and is deliberately not here — it needs a
rotation cursor so a truncated cycle does not starve the same tail every time,
and during a total kubelet outage there is nothing to collect anyway. The
symptom is latency in a cluster that is already broken.

10 seconds, 8 MiB and 64 MiB are proxies for "healthy" and will misjudge cases:
a kubelet on a heavily loaded node can take longer than 10 s to answer honestly,
and it will be recorded as a failure. The counters are cumulative, so the next
successful poll recovers the whole interval — the cost of a wrong guess here is
a data point in `polls_failed`, not a gap.

Raising the client's QPS raises the load the agent can put on an API server from
5 requests per second to 50. That is a ceiling, not a rate: the steady state is
`2N/30` requests per second and unchanged by this decision.

**Not changed.** Nothing collected, stored or transmitted. No payload, no golden
file, no RBAC grant, no chart value. The two call sites are still the same two
`GET`s through the proxy that `docs/security.md` §4 describes.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
