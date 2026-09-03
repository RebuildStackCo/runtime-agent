package metadata

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

func pod(namespace, name, node, phase, digest string, res model.Resources) model.PodInfo {
	return model.PodInfo{
		Namespace: namespace,
		Name:      name,
		Node:      node,
		Phase:     phase,
		QOSClass:  "Burstable",
		Workload:  model.WorkloadRef{Kind: "Deployment", Name: "api"},
		Containers: []model.Container{{
			Name:        "server",
			Image:       "registry.example.com/api:v1",
			ImageDigest: digest,
			Resources:   res,
		}},
	}
}

func burstable() model.Resources {
	return model.Resources{
		CPURequestMilli:    ptr.To[int64](250),
		MemoryRequestBytes: ptr.To[int64](128 << 20),
	}
}

func TestAggregateCountsReplicasPhasesAndNodes(t *testing.T) {
	got := Aggregate([]model.PodInfo{
		pod("acme", "api-1", "node-a", "Running", "sha256:aaa", burstable()),
		pod("acme", "api-2", "node-a", "Running", "sha256:aaa", burstable()),
		pod("acme", "api-3", "node-b", "Pending", "sha256:aaa", burstable()),
	}, nil)

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
	updated := model.Resources{
		CPURequestMilli:    ptr.To[int64](500),
		MemoryRequestBytes: ptr.To[int64](256 << 20),
	}
	got := Aggregate([]model.PodInfo{
		pod("acme", "api-old", "node-a", "Running", "sha256:aaa", old),
		pod("acme", "api-new", "node-b", "Running", "sha256:bbb", updated),
	}, nil)

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
	got := Aggregate([]model.PodInfo{
		pod("acme", "api-1", "node-a", "Running", "sha256:aaa", model.Resources{
			CPURequestMilli: ptr.To[int64](0),
		}),
	}, nil)

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
	got := Aggregate([]model.PodInfo{
		pod("acme", "api-1", "", "Pending", "", model.Resources{}),
	}, nil)

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

// The shortfall above says a replica is not running; this says why. A gated pod
// and an unschedulable one are both "no node", and only the reason separates a
// deliberate hold from a capacity problem (ADR 0021).
func TestAggregateCountsUnscheduledPodsByReason(t *testing.T) {
	unschedulable := pod("acme", "api-1", "", "Pending", "", model.Resources{})
	unschedulable.Unscheduled = "Unschedulable"
	gated := pod("acme", "api-2", "", "Pending", "", model.Resources{})
	gated.Unscheduled = "SchedulingGated"
	running := pod("acme", "api-3", "node-1", "Running", "", model.Resources{})

	got := Aggregate([]model.PodInfo{unschedulable, gated, running}, nil)

	rec := got[0]
	if rec.Pod.Replicas != 3 {
		t.Fatalf("replicas = %d, want 3", rec.Pod.Replicas)
	}
	want := map[string]int{"Unschedulable": 1, "SchedulingGated": 1}
	if !maps.Equal(rec.Pod.Unscheduled, want) {
		t.Errorf("unscheduled = %v, want %v", rec.Pod.Unscheduled, want)
	}
	// A scheduled replica contributes to neither the count nor the reasons.
	if len(rec.Pod.Nodes) != 1 {
		t.Errorf("nodes = %v, want just the scheduled replica's node", rec.Pod.Nodes)
	}
}

// The payload bytes are the schema contract, so aggregation must not depend on
// the order pods arrive in from the index (a Go map iteration).
func TestAggregateIsOrderIndependent(t *testing.T) {
	pods := []model.PodInfo{
		pod("zeta", "api-1", "node-b", "Running", "sha256:bbb", burstable()),
		pod("acme", "api-2", "node-a", "Running", "sha256:aaa", burstable()),
		pod("acme", "api-1", "node-c", "Running", "sha256:aaa", burstable()),
	}
	reversed := []model.PodInfo{pods[2], pods[1], pods[0]}

	first, err := json.Marshal(Aggregate(pods, nil))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(Aggregate(reversed, nil))
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
	withSidecar := func(name, node string) model.PodInfo {
		p := pod("acme", name, node, "Running", "sha256:aaa", burstable())
		p.Containers = append(p.Containers, model.Container{
			Name: "istio-proxy", Image: "istio/proxyv2:1.24.0", ImageDigest: "sha256:ccc",
		})
		return p
	}
	got := Aggregate([]model.PodInfo{
		withSidecar("api-1", "node-a"),
		withSidecar("api-2", "node-b"),
	}, nil)

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

// Terminated pods are reported, not dropped: a CronJob's workload legitimately
// carries dozens of Succeeded pods that consume nothing. Dropping them would be
// the agent deciding which phases count; reporting them in the breakdown lets
// the backend decide while keeping the raw replica count readable.
func TestAggregateReportsTerminatedPodsInTheBreakdown(t *testing.T) {
	got := Aggregate([]model.PodInfo{
		pod("acme", "job-1", "node-a", "Succeeded", "sha256:aaa", burstable()),
		pod("acme", "job-2", "node-a", "Failed", "sha256:aaa", burstable()),
		pod("acme", "job-3", "node-a", "Running", "sha256:aaa", burstable()),
	}, nil)

	rec := got[0]
	if rec.Pod.Replicas != 3 {
		t.Errorf("replicas = %d, want 3 — terminated pods are counted, not silently dropped", rec.Pod.Replicas)
	}
	want := map[string]int{"Succeeded": 1, "Failed": 1, "Running": 1}
	if !reflect.DeepEqual(rec.Pod.Phases, want) {
		t.Errorf("phases = %v, want %v — the breakdown is what makes the replica count readable",
			rec.Pod.Phases, want)
	}
}

// Init containers schedule differently from regular ones, so the flag must
// reach the payload and must not collapse two containers into one record.
func TestAggregateKeepsInitContainersDistinct(t *testing.T) {
	p := pod("acme", "api-1", "node-a", "Running", "sha256:aaa", burstable())
	p.Containers = append(p.Containers, model.Container{
		Name: "migrate", Image: "registry.example.com/migrate:v1",
		ImageDigest: "sha256:ccc", Init: true,
	})

	got := Aggregate([]model.PodInfo{p}, nil)
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

// Placement is a pod fact, so it lands in the pod block and repeats across the
// pod's containers exactly as the counts do (ADR 0014). It is taken from the
// pod, not derived here.
func TestAggregateCarriesPlacementIntoThePodBlock(t *testing.T) {
	p := pod("acme", "api-1", "node-a", "Running", "sha256:aaa", burstable())
	p.Placement = model.Placement{
		PodAntiAffinity: []model.TopologyTerm{
			{TopologyKey: "kubernetes.io/hostname", Required: true},
		},
		PriorityClass: "high",
	}

	got := Aggregate([]model.PodInfo{p}, nil)
	if len(got) != 1 {
		t.Fatalf("records = %d, want 1", len(got))
	}
	if got[0].Pod.Placement.PriorityClass != "high" {
		t.Errorf("priority class = %q", got[0].Pod.Placement.PriorityClass)
	}
	if len(got[0].Pod.Placement.PodAntiAffinity) != 1 {
		t.Errorf("anti-affinity = %+v", got[0].Pod.Placement.PodAntiAffinity)
	}
}

// A rollout that tightens placement must not have one build's constraints
// overwrite the other's. The record key carries the image digest, so the two
// builds are separate records and each keeps what it declared.
func TestAggregateKeepsEachBuildsPlacementDuringRollout(t *testing.T) {
	old := pod("acme", "api-1", "node-a", "Running", "sha256:aaa", burstable())
	old.Placement = model.Placement{PriorityClass: "normal"}
	fresh := pod("acme", "api-2", "node-b", "Running", "sha256:bbb", burstable())
	fresh.Placement = model.Placement{
		PriorityClass: "high",
		NodeSelector:  map[string]string{"pool": "reserved"},
	}

	got := Aggregate([]model.PodInfo{old, fresh}, nil)
	if len(got) != 2 {
		t.Fatalf("records = %d, want one per build", len(got))
	}
	byDigest := map[string]model.Placement{}
	for _, r := range got {
		byDigest[r.ImageDigest] = r.Pod.Placement
	}
	if byDigest["sha256:aaa"].PriorityClass != "normal" {
		t.Errorf("old build placement = %+v", byDigest["sha256:aaa"])
	}
	if byDigest["sha256:bbb"].NodeSelector["pool"] != "reserved" {
		t.Errorf("new build placement = %+v", byDigest["sha256:bbb"])
	}
}

// A workload with no constraints contributes no placement bytes at all: the
// block is omitted rather than written empty on every record.
func TestAggregateOmitsPlacementForAnUnconstrainedWorkload(t *testing.T) {
	got := Aggregate([]model.PodInfo{
		pod("acme", "api-1", "node-a", "Running", "sha256:aaa", burstable()),
	}, nil)
	encoded, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes := string(encoded); strings.Contains(bytes, "placement") {
		t.Errorf("unconstrained workload encoded a placement block:\n%s", bytes)
	}
}

// A workload whose strategy the agent did not read carries no `workload` object
// at all. A block of zeros would read as a strategy that replaces every replica
// at once, which is not what the agent observed (ADR 0048 §2).
func TestARecordWithNoStrategyCarriesNoWorkloadObject(t *testing.T) {
	withStrategy := Aggregate([]model.PodInfo{
		pod("acme", "api-1", "node-a", "Running", "sha256:aaa", burstable()),
	}, map[model.WorkloadKey]model.UpdateStrategy{
		{Namespace: "acme", Kind: "Deployment", Name: "api"}: {Type: "RollingUpdate"},
	})
	encoded, err := json.Marshal(withStrategy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"workload":{"update_strategy":{"type":"RollingUpdate"}}`) {
		t.Fatalf("a strategy the agent read must ship nested under workload: %s", encoded)
	}

	without := Aggregate([]model.PodInfo{
		pod("acme", "api-1", "node-a", "Running", "sha256:aaa", burstable()),
	}, nil)
	encoded, err = json.Marshal(without)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"workload"`) {
		t.Errorf("a record with no strategy still carries a workload object: %s", encoded)
	}
}
