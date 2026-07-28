package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func controllerRef(kind, name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "v1", Kind: kind, Name: name, UID: types.UID("uid-" + name),
		Controller: ptr.To(true),
	}
}

func pod(name string, owner *metav1.OwnerReference) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, UID: types.UID("uid-" + name)},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			InitContainers: []corev1.Container{
				{Name: "init-db", Image: "example.com/migrate:v3"},
			},
			Containers: []corev1.Container{
				{Name: "app", Image: "example.com/app:1.2.3"},
				{Name: "sidecar", Image: "example.com/proxy:latest"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if owner != nil {
		p.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return p
}

// collectPods runs a PodWatcher over the given objects and returns everything
// reported until at least want pods are seen or the timeout hits.
func collectPods(t *testing.T, want int, objects ...runtime.Object) map[string]PodInfo {
	t.Helper()
	clientset := fake.NewClientset(objects...)

	var mu sync.Mutex
	seen := make(map[string]PodInfo)
	got := make(chan struct{}, 64)
	watcher := NewPodWatcher(clientset, func(p PodInfo) {
		mu.Lock()
		seen[p.Namespace+"/"+p.Name] = p
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
			t.Fatalf("saw %d pods before timeout, want %d", n, want)
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

func TestResolvesWorkloadChains(t *testing.T) {
	rsOwnedByDeployment := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "checkout-7d9f", UID: "uid-checkout-7d9f",
		OwnerReferences: []metav1.OwnerReference{controllerRef("Deployment", "checkout")},
	}}
	rsOwnedByRollout := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "payments-5c4b", UID: "uid-payments-5c4b",
		OwnerReferences: []metav1.OwnerReference{controllerRef("Rollout", "payments")},
	}}
	jobOwnedByCronJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "report-29000", UID: "uid-report-29000",
		OwnerReferences: []metav1.OwnerReference{controllerRef("CronJob", "report")},
	}}
	bareJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "one-off", UID: "uid-one-off",
	}}

	seen := collectPods(t, 7,
		rsOwnedByDeployment, rsOwnedByRollout, jobOwnedByCronJob, bareJob,
		pod("checkout-7d9f-x1", ptr.To(controllerRef("ReplicaSet", "checkout-7d9f"))),
		pod("payments-5c4b-x1", ptr.To(controllerRef("ReplicaSet", "payments-5c4b"))),
		pod("report-29000-x1", ptr.To(controllerRef("Job", "report-29000"))),
		pod("one-off-x1", ptr.To(controllerRef("Job", "one-off"))),
		pod("db-0", ptr.To(controllerRef("StatefulSet", "db"))),
		pod("node-exporter-abc", ptr.To(controllerRef("DaemonSet", "node-exporter"))),
		pod("debug-shell", nil),
	)

	want := map[string]WorkloadRef{
		"shop/checkout-7d9f-x1":  {Kind: "Deployment", Name: "checkout"},
		"shop/payments-5c4b-x1":  {Kind: "Rollout", Name: "payments"},
		"shop/report-29000-x1":   {Kind: "CronJob", Name: "report"},
		"shop/one-off-x1":        {Kind: "Job", Name: "one-off"},
		"shop/db-0":              {Kind: "StatefulSet", Name: "db"},
		"shop/node-exporter-abc": {Kind: "DaemonSet", Name: "node-exporter"},
		"shop/debug-shell":       {Kind: "none"},
	}
	for key, workload := range want {
		info, ok := seen[key]
		if !ok {
			t.Errorf("pod %s was not reported", key)
			continue
		}
		if info.Workload != workload {
			t.Errorf("pod %s: workload = %+v, want %+v", key, info.Workload, workload)
		}
	}
}

func TestCollectsContainersAndImages(t *testing.T) {
	seen := collectPods(t, 1, pod("checkout-1", nil))

	info := seen["shop/checkout-1"]
	want := []Container{
		{Name: "init-db", Image: "example.com/migrate:v3", Init: true},
		{Name: "app", Image: "example.com/app:1.2.3"},
		{Name: "sidecar", Image: "example.com/proxy:latest"},
	}
	if len(info.Containers) != len(want) {
		t.Fatalf("containers = %+v, want %+v", info.Containers, want)
	}
	for i := range want {
		if info.Containers[i] != want[i] {
			t.Errorf("container %d = %+v, want %+v", i, info.Containers[i], want[i])
		}
	}
	if info.Node != "node-1" || info.Phase != string(corev1.PodRunning) {
		t.Errorf("node/phase = %q/%q, want node-1/Running", info.Node, info.Phase)
	}
}

func TestReportsPodsCreatedAfterStart(t *testing.T) {
	clientset := fake.NewClientset(pod("existing", nil))

	events := make(chan PodInfo, 16)
	watcher := NewPodWatcher(clientset, func(p PodInfo) { events <- p })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	waitFor := func(name string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case p := <-events:
				if p.Name == name {
					return
				}
			case <-deadline:
				t.Fatalf("pod %s was not reported before timeout", name)
			}
		}
	}

	waitFor("existing")

	newPod := pod("created-later", ptr.To(controllerRef("StatefulSet", "db")))
	if _, err := clientset.CoreV1().Pods("shop").Create(ctx, newPod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating pod: %v", err)
	}
	waitFor("created-later")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
}
