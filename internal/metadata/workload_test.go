package metadata

import (
	"encoding/json"
	"reflect"
	"testing"

	"k8s.io/utils/ptr"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

func pod(namespace, name, node, phase, digest string, res collector.Resources) collector.PodInfo {
	return collector.PodInfo{
		Namespace: namespace,
		Name:      name,
		Node:      node,
		Phase:     phase,
		QOSClass:  "Burstable",
		Workload:  collector.WorkloadRef{Kind: "Deployment", Name: "api"},
		Containers: []collector.Container{{
			Name:        "server",
			Image:       "registry.example.com/api:v1",
			ImageDigest: digest,
			Resources:   res,
		}},
	}
}

func burstable() collector.Resources {
	return collector.Resources{
		CPURequestMilli:    ptr.To[int64](250),
		MemoryRequestBytes: ptr.To[int64](128 << 20),
	}
}

func TestAggregateCountsReplicasPhasesAndNodes(t *testing.T) {
	got := Aggregate([]collector.PodInfo{
		pod("acme", "api-1", "node-a", "Running", "sha256:aaa", burstable()),
		pod("acme", "api-2", "node-a", "Running", "sha256:aaa", burstable()),
		pod("acme", "api-3", "node-b", "Pending", "sha256:aaa", burstable()),
	})

	if len(got) != 1 {
		t.Fatalf("records = %d, want 1 (same workload, same build)", len(got))
	}
	rec := got[0]
	if rec.Pod.Replicas != 3 {
		t.Errorf("replicas = %d, want 3", rec.Pod.Replicas)
	}
	if want := map[string]int{"Running": 2, "Pending": 1}; !reflect.DeepEqual(rec.Pod.Phases, want) {
		t.Errorf("phases = %v, want %v", rec.Pod.Phases, want)
	}
	if want := map[string]int{"node-a": 2, "node-b": 1}; !reflect.DeepEqual(rec.Pod.Nodes, want) {
		t.Errorf("nodes = %v, want %v", rec.Pod.Nodes, want)
	}
}

// A rollout runs two builds at once. Keying on the image digest reports both
// with their own declared resources instead of letting the pod observed last
// overwrite the other.
func TestAggregateSplitsBuildsDuringRollout(t *testing.T) {
	old := burstable()
	updated := collector.Resources{
		CPURequestMilli:    ptr.To[int64](500),
		MemoryRequestBytes: ptr.To[int64](256 << 20),
	}
	got := Aggregate([]collector.PodInfo{
		pod("acme", "api-old", "node-a", "Running", "sha256:aaa", old),
		pod("acme", "api-new", "node-b", "Running", "sha256:bbb", updated),
	})

	if len(got) != 2 {
		t.Fatalf("records = %d, want 2 (one per build)", len(got))
	}
	if got[0].ImageDigest != "sha256:aaa" || got[1].ImageDigest != "sha256:bbb" {
		t.Fatalf("digests = %q, %q; want sorted aaa, bbb", got[0].ImageDigest, got[1].ImageDigest)
	}
	if *got[0].Resources.CPURequestMilli != 250 || *got[1].Resources.CPURequestMilli != 500 {
		t.Errorf("each build must keep its own declared request, got %d and %d",
			*got[0].Resources.CPURequestMilli, *got[1].Resources.CPURequestMilli)
	}
}

// An unset request is not a zero request; the distinction survives into JSON,
// where nil is omitted and zero is present.
func TestAggregatePreservesUnsetVersusZero(t *testing.T) {
	got := Aggregate([]collector.PodInfo{
		pod("acme", "api-1", "node-a", "Running", "sha256:aaa", collector.Resources{
			CPURequestMilli: ptr.To[int64](0),
		}),
	})

	rec := got[0]
	if rec.Resources.CPURequestMilli == nil || *rec.Resources.CPURequestMilli != 0 {
		t.Fatalf("cpu request = %v, want an explicit 0", rec.Resources.CPURequestMilli)
	}
	if rec.Resources.CPULimitMilli != nil {
		t.Fatalf("cpu limit = %v, want nil (unset)", rec.Resources.CPULimitMilli)
	}
	encoded, err := json.Marshal(rec.Resources)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["cpu_request_milli"]; !ok {
		t.Error("an explicit zero request must appear in the payload")
	}
	if _, ok := fields["cpu_limit_milli"]; ok {
		t.Error("an unset limit must be absent from the payload, not zero")
	}
}

// A pod that has not been scheduled has no node, so Nodes sums to fewer than
// Replicas. That shortfall is the pending-scheduling signal, not a gap.
func TestAggregateUnscheduledPodCountsAsReplicaWithoutNode(t *testing.T) {
	got := Aggregate([]collector.PodInfo{
		pod("acme", "api-1", "", "Pending", "", collector.Resources{}),
	})

	rec := got[0]
	if rec.Pod.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", rec.Pod.Replicas)
	}
	if len(rec.Pod.Nodes) != 0 {
		t.Errorf("nodes = %v, want empty for an unscheduled pod", rec.Pod.Nodes)
	}
	if rec.ImageDigest != "" {
		t.Errorf("image digest = %q, want empty before the container starts", rec.ImageDigest)
	}
}

// The payload bytes are the schema contract, so aggregation must not depend on
// the order pods arrive in from the index (a Go map iteration).
func TestAggregateIsOrderIndependent(t *testing.T) {
	pods := []collector.PodInfo{
		pod("zeta", "api-1", "node-b", "Running", "sha256:bbb", burstable()),
		pod("acme", "api-2", "node-a", "Running", "sha256:aaa", burstable()),
		pod("acme", "api-1", "node-c", "Running", "sha256:aaa", burstable()),
	}
	reversed := []collector.PodInfo{pods[2], pods[1], pods[0]}

	first, err := json.Marshal(Aggregate(pods))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(Aggregate(reversed))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("aggregation is order-dependent:\n%s\n%s", first, second)
	}
}

// A pod with several containers produces one record per container, and each
// repeats the same pod facts — the pods are the same pods, counted once per
// record and never multiplied by the container count. The nesting is what makes
// that legible: pod.replicas on three records is obviously one pod fact seen
// three times, where a flat replicas field invites a sum.
func TestAggregatePodFactsRepeatPerContainerWithoutMultiplying(t *testing.T) {
	withSidecar := func(name, node string) collector.PodInfo {
		p := pod("acme", name, node, "Running", "sha256:aaa", burstable())
		p.Containers = append(p.Containers, collector.Container{
			Name: "istio-proxy", Image: "istio/proxyv2:1.24.0", ImageDigest: "sha256:ccc",
		})
		return p
	}
	got := Aggregate([]collector.PodInfo{
		withSidecar("api-1", "node-a"),
		withSidecar("api-2", "node-b"),
	})

	if len(got) != 2 {
		t.Fatalf("records = %d, want 2 (one per container of the same two pods)", len(got))
	}
	for _, rec := range got {
		if rec.Pod.Replicas != 2 {
			t.Errorf("%s: pod.replicas = %d, want 2 — each record reports the same two pods",
				rec.Container, rec.Pod.Replicas)
		}
		if !reflect.DeepEqual(rec.Pod, got[0].Pod) {
			t.Errorf("%s: pod block = %+v, want identical to %+v across containers of one pod",
				rec.Container, rec.Pod, got[0].Pod)
		}
	}
}

// Init containers schedule differently from regular ones, so the flag must
// reach the payload and must not collapse two containers into one record.
func TestAggregateKeepsInitContainersDistinct(t *testing.T) {
	p := pod("acme", "api-1", "node-a", "Running", "sha256:aaa", burstable())
	p.Containers = append(p.Containers, collector.Container{
		Name: "migrate", Image: "registry.example.com/migrate:v1",
		ImageDigest: "sha256:ccc", Init: true,
	})

	got := Aggregate([]collector.PodInfo{p})
	if len(got) != 2 {
		t.Fatalf("records = %d, want 2 (init and regular container)", len(got))
	}
	var init *Record
	for i := range got {
		if got[i].Container == "migrate" {
			init = &got[i]
		}
	}
	if init == nil || !init.Init {
		t.Fatalf("init container record = %+v, want Init true", init)
	}
}
