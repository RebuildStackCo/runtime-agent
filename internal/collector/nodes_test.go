package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func node(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{KernelVersion: "6.1.0-generic"},
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3900m"),
				corev1.ResourceMemory: resource.MustParse("14Gi"),
			},
		},
	}
}

// collectNodes runs a NodeWatcher over the given objects and returns
// everything reported until at least want nodes are seen or the timeout hits.
func collectNodes(t *testing.T, want int, objects ...runtime.Object) map[string]NodeInfo {
	t.Helper()
	clientset := fake.NewClientset(objects...)

	var mu sync.Mutex
	seen := make(map[string]NodeInfo)
	got := make(chan struct{}, 64)
	watcher := NewNodeWatcher(clientset, func(n NodeInfo) {
		mu.Lock()
		seen[n.Name] = n
		mu.Unlock()
		got <- struct{}{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for n := 0; n < want; {
		select {
		case <-got:
			n++
		case <-deadline:
			t.Fatalf("saw %d nodes before timeout, want %d", n, want)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return seen
}

func TestCollectsNodeSizes(t *testing.T) {
	seen := collectNodes(t, 1, node("node-1", nil))

	info := seen["node-1"]
	want := NodeInfo{
		Name:                   "node-1",
		KernelVersion:          "6.1.0-generic",
		AllocatableCPUMilli:    3900,
		AllocatableMemoryBytes: 14 << 30,
		CapacityCPUMilli:       4000,
		CapacityMemoryBytes:    16 << 30,
	}
	if info != want {
		t.Errorf("node = %+v, want %+v", info, want)
	}
}

func TestCollectsKernelVersion(t *testing.T) {
	n := node("node-1", nil)
	n.Status.NodeInfo.KernelVersion = "5.15.0-91-generic"
	seen := collectNodes(t, 1, n)

	if got := seen["node-1"].KernelVersion; got != "5.15.0-91-generic" {
		t.Errorf("kernel version = %q, want 5.15.0-91-generic", got)
	}
}

func TestNodeInstanceAndCapacityTypes(t *testing.T) {
	cases := []struct {
		name             string
		labels           map[string]string
		wantInstanceType string
		wantCapacityType string
	}{
		{
			name: "karpenter-spot",
			labels: map[string]string{
				"node.kubernetes.io/instance-type": "m6i.large",
				"karpenter.sh/capacity-type":       "spot",
			},
			wantInstanceType: "m6i.large",
			wantCapacityType: "spot",
		},
		{
			name: "eks-on-demand",
			labels: map[string]string{
				"node.kubernetes.io/instance-type": "m5.xlarge",
				"eks.amazonaws.com/capacityType":   "ON_DEMAND",
			},
			wantInstanceType: "m5.xlarge",
			wantCapacityType: "on-demand",
		},
		{
			name: "gke-spot",
			labels: map[string]string{
				"node.kubernetes.io/instance-type": "e2-standard-4",
				"cloud.google.com/gke-spot":        "true",
			},
			wantInstanceType: "e2-standard-4",
			wantCapacityType: "spot",
		},
		{
			name: "gke-preemptible",
			labels: map[string]string{
				"cloud.google.com/gke-preemptible": "true",
			},
			wantCapacityType: "spot",
		},
		{
			name: "aks-regular",
			labels: map[string]string{
				"kubernetes.azure.com/scalesetpriority": "regular",
			},
			wantCapacityType: "on-demand",
		},
		{
			name: "beta-instance-type-fallback",
			labels: map[string]string{
				"beta.kubernetes.io/instance-type": "t3.medium",
			},
			wantInstanceType: "t3.medium",
		},
		{
			name:   "no-markers",
			labels: map[string]string{"kubernetes.io/hostname": "bare-metal-7"},
		},
	}

	objects := make([]runtime.Object, 0, len(cases))
	for _, c := range cases {
		objects = append(objects, node(c.name, c.labels))
	}
	seen := collectNodes(t, len(cases), objects...)

	for _, c := range cases {
		info, ok := seen[c.name]
		if !ok {
			t.Errorf("node %s was not reported", c.name)
			continue
		}
		if info.InstanceType != c.wantInstanceType {
			t.Errorf("%s: instance type = %q, want %q", c.name, info.InstanceType, c.wantInstanceType)
		}
		if info.CapacityType != c.wantCapacityType {
			t.Errorf("%s: capacity type = %q, want %q", c.name, info.CapacityType, c.wantCapacityType)
		}
	}
}

func TestReportsNodesAddedAfterStart(t *testing.T) {
	clientset := fake.NewClientset(node("existing", nil))

	events := make(chan NodeInfo, 16)
	watcher := NewNodeWatcher(clientset, func(n NodeInfo) { events <- n })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	waitFor := func(name string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case n := <-events:
				if n.Name == name {
					return
				}
			case <-deadline:
				t.Fatalf("node %s was not reported before timeout", name)
			}
		}
	}

	waitFor("existing")

	if _, err := clientset.CoreV1().Nodes().Create(ctx, node("scaled-up", nil), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating node: %v", err)
	}
	waitFor("scaled-up")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
}
