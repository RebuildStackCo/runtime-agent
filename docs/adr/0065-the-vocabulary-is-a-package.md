# 0065. The vocabulary of a collected fact is a package, and it does not know Kubernetes

Date: 2026-09-03

Status: Accepted

Moves the plain-data types that describe collected facts — `WorkloadRef`,
`PodInfo`, `NodeInfo`, the journal events, the policy reductions, the coverage
counters — out of `internal/collector` into `internal/model`. Nothing else
changes: the golden payload bytes are identical, which is the property that
makes a change this wide reviewable at all.

## Context

`internal/collector` had grown into three packages sharing one name: the
vocabulary of collected facts, the Kubernetes adapter that fills it in
(informers, listers, the kubelet poller), and the reductions that shrink an
object to what ships. Only the middle one needs a cluster.

The vocabulary is what every other package speaks. `journal` windows restart
counts, `metadata` aggregates pod shapes, `revisions` orders ReplicaSets,
`sink` serializes all of it — none of them opens a connection, and every one of
them imported `collector` to name a type. Measured on the tree before this
change:

| package | packages in its transitive closure | of them client-go |
| --- | --- | --- |
| `rollup` | 1 | 0 |
| `journal`, `metadata`, `revisions` | 458 | 261 |
| `sink` | 486 | 261 |

`rollup` is the control. It is window arithmetic that never needed to name a
workload, so it stayed free — which is the evidence that the weight above is
not the price of the domain but the price of where the names happened to live.
Nothing about windowing a restart count requires 261 packages of Kubernetes
client; it requires the identifier `WorkloadRef`.

Build weight is the symptom and not the reason. The reason is that no boundary
stood between a Kubernetes type and a payload struct. `PodInfo` and its
neighbours were already free of `corev1` — every field is a string, a number or
a pointer to one — but that was a convention held up by review, in the one
direction where a lapse is not a refactor but a wire-format change. The
extractors that read `corev1` and the structs they fill sat in the same files,
so the only thing separating them was which line a reader was on.

## Decision

`internal/model` holds the vocabulary: 48 exported declarations, 894 lines, no
imports beyond `time` and `reflect`. A test in the package asserts that last
part, because it is the whole point and nothing else in the build would fail if
it stopped being true.

`internal/collector` keeps what needs a cluster — the watchers, the kubelet
poller, the filter, and every reduction from an API object down to a model type
— and drops from 5801 to 4946 lines and from 95 to 48 exported declarations. It
is still three packages in one; splitting the adapter from the reductions is a
separate change this one does not make.

Two methods could not travel unchanged, and the split is along the same seam:

- `NodeInfo.sameAs` became `model.NodeInfo.SameAs`. Whether two collected views
  are the same fact is a question about the type, so it goes with the type, and
  exporting it is the cost of that.
- `ResourceAmounts.empty` became `amountsEmpty` in `collector`. Whether a
  reduced amount set is worth emitting is a question the reduction asks, not a
  property of the value.

Alternatives considered and rejected:

- **Aliases in `collector`** (`type WorkloadRef = model.WorkloadRef`) would have
  made the diff a tenth of the size and let consumers migrate one at a time. It
  leaves two names for one type — and it leaves `journal` free to keep importing
  `collector`, which is the thing being prevented. A boundary that permits the
  old path is a suggestion.
- **Splitting `collector` by concern first** (watch / kubelet / reduce) is the
  larger and more interesting change, but each of those parts still needs the
  vocabulary, so this one comes first regardless.
- **Leaving it** was viable — nothing was broken. It is rejected because the
  boundary that was missing is the one between "what the cluster said" and "what
  we ship", and that is the boundary the whole agent is about.

## Consequences

The packages that reduce, window and serialize facts no longer link a
Kubernetes client: `journal`, `metadata` and `revisions` go from 458 packages
to 2, `sink` from 486 to 37, and `inventory`, `targeting` and `pprofprobe` fall
with them. `cmd/agent` is now the only package that imports `collector`.

A payload type can no longer quietly acquire a `corev1` field, because the
package it lives in cannot see `corev1`.

What is given up: one more hop. A reader of `collector` who wants a type's
definition now changes files, and a fact's shape and the code that fills it are
no longer adjacent. The doc comments went with the types, so the explanation
travels with the shape rather than with the extraction — which is the right half
to keep them next to, but it is a real loss for whoever is reading the extractor.
