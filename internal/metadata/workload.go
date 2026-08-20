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
	// QOSClass is the pod-level class the kubelet assigned. It is a property of
	// the whole pod, denormalized onto each of its containers; pods sharing this
	// record's key share their spec, so they share the class.
	QOSClass string `json:"qos_class,omitempty"`
	// Resources preserves the distinction between an unset request or limit
	// (nil) and one explicitly set to zero — the difference is the whole point
	// of several findings, and JSON omits nil rather than flattening it.
	Resources collector.Resources `json:"resources"`
	// Ports are the container's declared ports. Declaring a port is not using
	// it; that inference belongs to the backend.
	Ports []collector.ContainerPort `json:"ports,omitempty"`
	// Replicas is how many collected pods currently carry this container of
	// this build.
	Replicas int `json:"replicas"`
	// Phases counts those replicas by pod phase. It sums to Replicas, and it is
	// what keeps "ten replicas running" distinguishable from "ten replicas
	// pending" without the agent deciding which matters.
	Phases map[string]int `json:"phases,omitempty"`
	// Nodes counts those replicas by node name — the join to node metadata, and
	// through it to zone. Unscheduled pods have no node yet, so Nodes may sum to
	// fewer than Replicas; that shortfall is itself the pending-scheduling
	// signal, not a gap in collection.
	Nodes map[string]int `json:"nodes,omitempty"`
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
					QOSClass:  pod.QOSClass,
					Resources: c.Resources,
					Ports:     c.Ports,
					Phases:    make(map[string]int),
					Nodes:     make(map[string]int),
				}
				byKey[key] = rec
			}
			rec.Replicas++
			if pod.Phase != "" {
				rec.Phases[pod.Phase]++
			}
			if pod.Node != "" {
				rec.Nodes[pod.Node]++
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
