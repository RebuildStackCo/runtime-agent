// Package metadata reduces the controller's live pod index to the workload
// metadata payload: the declared shape of every collected workload container —
// requests, limits, QoS, declared ports, runtime knobs — and where its replicas
// currently run.
//
// Reduction, not judgment: nothing here compares a request to an observation or
// decides that anything is misconfigured. The package holds no state — every
// snapshot is derived from the pod index (ADR 0003).
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
// rollout runs two builds at once with possibly different declared resources;
// keying on the digest reports both instead of letting the last one seen win.
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
	// sums to Replicas and is what makes Replicas safe to read: a CronJob's
	// workload legitimately reports dozens of Succeeded pods that consume
	// nothing, and the breakdown lets a consumer separate them.
	//
	// Phase is placement and lifecycle, never health — a pod in
	// CrashLoopBackOff is Running. Readiness lives in status.conditions, which
	// this payload does not carry.
	Phases map[string]int `json:"phases,omitempty"`
	// Nodes counts those replicas by node name — the join to node metadata, and
	// through it to zone. Unscheduled pods have no node yet, so Nodes may sum to
	// fewer than Replicas; that shortfall is itself the pending-scheduling
	// signal, not a gap in collection.
	Nodes map[string]int `json:"nodes,omitempty"`
	// Unscheduled counts the replicas not yet on a node, by the reason the
	// scheduler gave — the shortfall above was always visible, the cause was
	// not (ADR 0021).
	//
	// The reasons are not interchangeable: "Unschedulable" is a capacity fact,
	// "SchedulingGated" is a deliberate hold that would invent a shortage if
	// counted as pressure, "SchedulerError" is the pod's own spec. The agent
	// reports which and draws no conclusion (ADR 0004).
	Unscheduled map[string]int `json:"unscheduled,omitempty"`
	// Placement is what the pod spec says about where these replicas may run
	// and what it costs to move them — the one thing usage, resources and node
	// metadata do not say.
	//
	// Taken from the first pod seen under the key, like Image and Resources
	// above. The key carries the image digest, so a rollout that changes
	// placement produces one record per build (ADR 0031).
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
	// RuntimeEnv is the Go runtime knobs this container sets, from the closed
	// list in ADR 0047. Absent means none of them is set, which is the state
	// most findings about this field are about.
	RuntimeEnv map[string]string `json:"runtime_env,omitempty"`
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
					Key:        key,
					Image:      c.Image,
					Init:       c.Init,
					Resources:  c.Resources,
					Ports:      c.Ports,
					RuntimeEnv: c.RuntimeEnv,
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
