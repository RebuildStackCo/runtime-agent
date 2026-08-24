// Package metadata reduces the controller's live pod index to the workload
// metadata payload: the declared shape of every collected workload container —
// requests, limits, QoS, declared ports — and where its replicas currently run.
//
// It is reduction, not judgment (CLAUDE.md: the agent's intelligence is data
// reduction). Nothing here compares a request to an observation, ranks a
// workload, or decides that anything is misconfigured; those are backend
// renderings of these facts. The package holds no state: every snapshot is
// derived from the pod index, so it is loss-harmless by construction (ADR 0003).
package metadata

import (
	"sort"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

// Key identifies one workload-metadata record: one container of one workload,
// of one build.
//
// The first four fields are exactly rollup.Key, so a usage record joins to its
// declared envelope without a lookup table. ImageDigest extends it because a
// rollout runs two builds of the same workload at once, and their declared
// resources may differ: keying on the digest reports both truthfully instead of
// letting whichever pod was observed last overwrite the other. The extra
// records collapse back to one when the rollout finishes.
type Key struct {
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workload_kind"`
	WorkloadName string `json:"workload_name"`
	Container    string `json:"container"`
	// ImageDigest is empty until the container starts — the runtime only knows
	// the digest after pulling the image. An empty digest is a real state
	// (declared but not yet running), not a missing value.
	ImageDigest string `json:"image_digest,omitempty"`
}

// PodScope holds the facts of this record that belong to the pod rather than to
// the container the record is keyed by. They live in their own object so their
// scope is visible in the payload itself: a pod with three containers produces
// three records, and each repeats the same pod facts. Flattened onto the record,
// `replicas: 2` on three container records reads as six replicas to anyone
// summing them — plausible, wrong, and with no failing test to catch it. Nested,
// summing `pod.replicas` across a pod's containers is self-evidently
// meaningless.
type PodScope struct {
	// QOSClass is assigned by the kubelet from the whole pod's containers, so
	// it is a property of the pod even though every container shares it.
	QOSClass string `json:"qos_class,omitempty"`
	// Replicas is how many collected pods currently carry this container of
	// this build.
	Replicas int `json:"replicas"`
	// Phases counts those replicas by pod phase (status.phase, verbatim). It
	// sums to Replicas, and it is what keeps "ten replicas running"
	// distinguishable from "ten replicas pending" without the agent deciding
	// which matters. It is also what makes Replicas safe to read: a CronJob's
	// workload legitimately reports dozens of Succeeded pods that consume
	// nothing, and the breakdown is what lets a consumer separate them instead
	// of the agent silently dropping them.
	//
	// Phase is placement and lifecycle, never health. "Running" means the pod
	// is bound to a node with at least one container started — a pod in
	// CrashLoopBackOff is Running, and so is one whose readiness probe has
	// never passed. Readiness lives in status.conditions and container
	// statuses, which this payload does not carry.
	Phases map[string]int `json:"phases,omitempty"`
	// Nodes counts those replicas by node name — the join to node metadata, and
	// through it to zone. Unscheduled pods have no node yet, so Nodes may sum to
	// fewer than Replicas; that shortfall is itself the pending-scheduling
	// signal, not a gap in collection.
	Nodes map[string]int `json:"nodes,omitempty"`
	// Unscheduled counts the replicas not yet on a node, by the reason the
	// scheduler gave. It explains the Nodes shortfall above rather than
	// restating it: the shortfall was always visible, the cause was not
	// (ADR 0021).
	//
	// The reasons are not interchangeable. "Unschedulable" means nothing in the
	// cluster fits the pod, which is a capacity fact. "SchedulingGated" means
	// the pod is deliberately held by a gate and is not waiting on capacity at
	// all — counting it as pressure would invent a shortage. "SchedulerError"
	// is the scheduler failing on the pod's own spec. The agent reports which,
	// and draws no conclusion (ADR 0004).
	Unscheduled map[string]int `json:"unscheduled,omitempty"`
	// Placement is what the pod spec says about where these replicas may run
	// and what it costs to move them. Usage says what a workload consumes,
	// resources say what it asked for, nodes say what machine it got — and
	// none of them says why it cannot be put somewhere cheaper. This does.
	//
	// It is taken from the first pod seen under the key, the same way Image,
	// Resources and Ports above are. The key carries the image digest, so a
	// rollout that changes placement produces one record per build, each with
	// its own constraints, rather than one record with whichever pod was seen
	// first (ADR 0031).
	Placement collector.Placement `json:"placement,omitzero"`
}

// Record is the declared shape of one workload container plus the placement of
// the replicas that carry it. Every field is copied from the pod spec or
// status; none is derived from a threshold.
type Record struct {
	Key
	// Image is the reference as written in the spec (tag or digest form). It is
	// kept alongside the resolved digest because a tag is what a human
	// recognizes and what a GitOps diff shows.
	Image string `json:"image"`
	// Init marks an init container. Its resources participate in scheduling
	// differently from a regular container's, so the distinction must survive.
	Init bool `json:"init,omitempty"`
	// Resources preserves the distinction between an unset request or limit
	// (nil) and one explicitly set to zero — the difference is the whole point
	// of several findings, and JSON omits nil rather than flattening it.
	Resources collector.Resources `json:"resources"`
	// Ports are the container's declared ports. Declaring a port is not using
	// it; that inference belongs to the backend.
	Ports []collector.ContainerPort `json:"ports,omitempty"`
	// Pod carries the facts that belong to the pod, not to this container.
	Pod PodScope `json:"pod"`
}

// Aggregate reduces a pod snapshot to one record per (workload container,
// build). Input order does not affect the output: records are sorted by key and
// the per-record maps marshal with sorted keys, so the payload bytes are
// deterministic — the golden contract (docs/development.md).
//
// Only pods the filter admitted reach this function; the identity of an
// excluded pod never appears here, in keeping with CLAUDE.md invariant 6.
func Aggregate(pods []collector.PodInfo) []Record {
	byKey := make(map[Key]*Record)
	for _, pod := range pods {
		for _, c := range pod.Containers {
			key := Key{
				Namespace:    pod.Namespace,
				WorkloadKind: pod.Workload.Kind,
				WorkloadName: pod.Workload.Name,
				Container:    c.Name,
				ImageDigest:  c.ImageDigest,
			}
			rec, ok := byKey[key]
			if !ok {
				rec = &Record{
					Key:       key,
					Image:     c.Image,
					Init:      c.Init,
					Resources: c.Resources,
					Ports:     c.Ports,
					Pod: PodScope{
						QOSClass:    pod.QOSClass,
						Placement:   pod.Placement,
						Phases:      make(map[string]int),
						Nodes:       make(map[string]int),
						Unscheduled: make(map[string]int),
					},
				}
				byKey[key] = rec
			}
			rec.Pod.Replicas++
			if pod.Phase != "" {
				rec.Pod.Phases[pod.Phase]++
			}
			if pod.Node != "" {
				rec.Pod.Nodes[pod.Node]++
			}
			if pod.Unscheduled != "" {
				rec.Pod.Unscheduled[pod.Unscheduled]++
			}
		}
	}

	out := make([]Record, 0, len(byKey))
	for _, rec := range byKey {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool { return lessKey(out[i].Key, out[j].Key) })
	return out
}

func lessKey(a, b Key) bool {
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.WorkloadKind != b.WorkloadKind {
		return a.WorkloadKind < b.WorkloadKind
	}
	if a.WorkloadName != b.WorkloadName {
		return a.WorkloadName < b.WorkloadName
	}
	if a.Container != b.Container {
		return a.Container < b.Container
	}
	return a.ImageDigest < b.ImageDigest
}
