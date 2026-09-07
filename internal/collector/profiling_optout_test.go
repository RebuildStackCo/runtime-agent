package collector

import (
	"context"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestAdmitProfilingSemantics(t *testing.T) {
	podIn := func(annotations map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "p", Annotations: annotations,
		}}
	}
	optOut := map[string]string{ProfileAnnotation: "false"}

	cases := []struct {
		name         string
		pod          *corev1.Pod
		nsAnn        map[string]string
		workload     WorkloadLookup
		wantProfiled bool
	}{
		{name: "no annotation profiles", pod: podIn(nil), wantProfiled: true},
		{name: "namespace opts out", pod: podIn(nil), nsAnn: optOut},
		{name: "workload opts out", pod: podIn(nil), workload: WorkloadLookup{Annotations: optOut}},
		{name: "pod opts out", pod: podIn(optOut)},
		// Only "false" is an opt-out, exactly as the collection annotation
		// beside it: one vocabulary, not two (ADR 0071).
		{name: "another value profiles", pod: podIn(map[string]string{ProfileAnnotation: "yes"}), wantProfiled: true},
		{name: "the collection annotation is not this one", pod: podIn(map[string]string{CollectAnnotation: "false"}), wantProfiled: true},
		// Fail-open on an unreadable controller, for ADR 0028's reason: it is
		// not evidence that anyone opted out, whichever annotation is sought.
		{name: "unknown workload kind still profiles", pod: podIn(nil), workload: WorkloadLookup{Unresolved: WorkloadKindUnknown}, wantProfiled: true},
		{name: "uncached workload still profiles", pod: podIn(nil), workload: WorkloadLookup{Unresolved: WorkloadNotCached}, wantProfiled: true},
		{name: "unreadable workload does not override the pod's own opt-out", pod: podIn(optOut), workload: WorkloadLookup{Unresolved: WorkloadKindUnknown}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NewFilter(nil, nil).AdmitProfiling(c.pod, c.nsAnn, c.workload); got != c.wantProfiled {
				t.Errorf("AdmitProfiling = %v, want %v", got, c.wantProfiled)
			}
		})
	}
}

// profilingWatcher runs a watcher over the objects until the filter has seen
// wantPods admissions, then stops it and hands the watcher back for inspection.
func profilingWatcher(t *testing.T, wantPods int64, objects ...*corev1.Pod) (*PodWatcher, *Filter) {
	t.Helper()
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web-7d9f", UID: "uid-web-7d9f",
		OwnerReferences: []metav1.OwnerReference{controllerRef("Deployment", "web")},
	}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web", UID: "uid-web",
	}}
	objs := []runtime.Object{rs, deployment}
	for _, p := range objects {
		objs = append(objs, p)
	}
	clientset := fake.NewClientset(objs...)

	watcher := NewPodWatcher(clientset, func(model.PodInfo) {})
	filter := NewFilter(nil, nil)
	watcher.SetFilter(filter)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = watcher.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for filter.Snapshot().PodsObserved < wantPods {
		if time.Now().After(deadline) {
			t.Fatalf("filter saw %+v before timeout, want %d pods", filter.Snapshot(), wantPods)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return watcher, filter
}

// profilingPod is the shared fixture: one replica of `shop/Deployment web`,
// running, addressable, with a started container of a known build.
func profilingPod(name, ip string, annotations map[string]string) *corev1.Pod {
	owner := controllerRef("ReplicaSet", "web-7d9f")
	p := pod(name, &owner)
	p.Annotations = annotations
	p.Status.PodIP = ip
	p.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "app", ContainerID: "containerd://" + name, ImageID: "example.com/app@sha256:d"},
	}
	return p
}

// The control excludes profiling and nothing else: the pod stays collected, and
// the two places that hand a pod to a profiler have nothing to hand (ADR 0071).
func TestAnOptedOutWorkloadIsCollectedAndNotProfiled(t *testing.T) {
	optOut := map[string]string{ProfileAnnotation: "false"}
	watcher, filter := profilingWatcher(t, 1, profilingPod("web-a", "10.0.0.1", optOut))

	if c := filter.Snapshot(); c.PodsObserved != 1 || c.ExcludedProfilingAnnotation != 1 {
		t.Errorf("coverage = %+v, want 1 pod observed and 1 excluded from profiling", c)
	}
	if pods := watcher.Pods(); len(pods) != 1 {
		t.Errorf("collected pods = %d, want the pod to stay collected", len(pods))
	}
	if cs := watcher.ContainersOnNode("node-1"); len(cs) != 0 {
		t.Errorf("targets answer names %+v, want nothing for an opted-out pod", cs)
	}
	web := model.WorkloadRef{Kind: "Deployment", Name: "web"}
	if addr, ok := watcher.PodAddress("shop", web, "app", "sha256:d"); ok {
		t.Errorf("PodAddress = %q, want no address to connect to", addr)
	}
	if _, ok := watcher.ProfilingOptOuts()[WorkloadKey{Namespace: "shop", Workload: web}]; !ok {
		t.Error("ProfilingOptOuts does not name the workload every replica of which opted out")
	}
}

// A pod-level opt-out is honored per pod: its containers are not named and it is
// not the replica asked, while the workload beside it keeps being profiled.
func TestOneOptedOutReplicaLeavesTheWorkloadProfilable(t *testing.T) {
	optOut := map[string]string{ProfileAnnotation: "false"}
	watcher, filter := profilingWatcher(t, 2,
		profilingPod("web-a", "10.0.0.1", optOut),
		profilingPod("web-b", "10.0.0.2", nil))

	if c := filter.Snapshot(); c.ExcludedProfilingAnnotation != 1 {
		t.Errorf("coverage = %+v, want exactly the annotated replica excluded", c)
	}
	cs := watcher.ContainersOnNode("node-1")
	if len(cs) != 1 || cs[0].ContainerID != "web-b" {
		t.Errorf("targets answer = %+v, want only the container of web-b", cs)
	}
	web := model.WorkloadRef{Kind: "Deployment", Name: "web"}
	if addr, ok := watcher.PodAddress("shop", web, "app", "sha256:d"); !ok || addr != "10.0.0.2" {
		t.Errorf("PodAddress = (%q, %v), want the replica that did not opt out", addr, ok)
	}
	if outs := watcher.ProfilingOptOuts(); len(outs) != 0 {
		t.Errorf("ProfilingOptOuts = %+v, want empty: one replica is not the workload", outs)
	}
}

// A workload with no indexed pod said nothing. Reporting it as opted out would
// credit the customer's annotation for a scale to zero.
func TestProfilingOptOutsNamesNoWorkloadWithoutPods(t *testing.T) {
	watcher, _ := profilingWatcher(t, 1, profilingPod("web-b", "10.0.0.2", nil))
	if outs := watcher.ProfilingOptOuts(); len(outs) != 0 {
		t.Errorf("ProfilingOptOuts = %+v, want empty", outs)
	}
}
