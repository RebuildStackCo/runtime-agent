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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// A customer annotates a pod that is already running, so the answer changes
// without the pod appearing again. The counters have to move with it: ADR 0054
// accepts that a workload leaving the collected set is visible by subtraction,
// and rests on the count explaining why — a record that vanishes while every
// number holds still reads to the backend as a workload that was deleted.

// transitionWatcher runs a watcher over one pod and returns once the filter has
// decided about it, handing back everything needed to change the pod underneath.
func transitionWatcher(t *testing.T, annotations map[string]string) (*PodWatcher, *Filter, kubernetes.Interface, *corev1.Pod) {
	t.Helper()
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web-7d9f", UID: "uid-web-7d9f",
		OwnerReferences: []metav1.OwnerReference{controllerRef("Deployment", "web")},
	}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web", UID: "uid-web",
	}}
	p := profilingPod("web-a", "10.0.0.1", annotations)
	clientset := fake.NewClientset([]runtime.Object{rs, deployment, p}...)

	watcher := NewPodWatcher(clientset, func(model.PodInfo) {})
	filter := NewFilter(nil, nil)
	watcher.SetFilter(filter)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = watcher.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		c := filter.Snapshot()
		if c.PodsObserved+c.ExcludedPodAnnotation > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the filter decided nothing about the pod before timeout: %+v", c)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return watcher, filter, clientset, p
}

// setAnnotations replaces the pod's annotations through the API the informer
// watches, which is what a customer editing a live pod does.
func setAnnotations(t *testing.T, cs kubernetes.Interface, p *corev1.Pod, annotations map[string]string) {
	t.Helper()
	ctx := context.Background()
	live, err := cs.CoreV1().Pods(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading %s/%s: %v", p.Namespace, p.Name, err)
	}
	live.Annotations = annotations
	if _, err := cs.CoreV1().Pods(p.Namespace).Update(ctx, live, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating %s/%s: %v", p.Namespace, p.Name, err)
	}
}

func TestOptingOutARunningPodIsCounted(t *testing.T) {
	watcher, filter, cs, p := transitionWatcher(t, nil)
	if c := filter.Snapshot(); c.PodsObserved != 1 || c.ExcludedPodAnnotation != 0 {
		t.Fatalf("coverage before the annotation = %+v, want 1 observed and 0 excluded", c)
	}

	setAnnotations(t, cs, p, map[string]string{CollectAnnotation: "false"})
	waitFor(t, 5*time.Second, "the opt-out to be counted",
		func() bool { return filter.Snapshot().ExcludedPodAnnotation == 1 })

	if pods := watcher.Pods(); len(pods) != 0 {
		t.Errorf("collected pods = %d, want the opted-out pod dropped", len(pods))
	}
}

// The pod goes on emitting status updates after the opt-out, each carrying the
// same answer. Counting the state rather than the change would climb forever.
func TestRepeatedUpdatesAfterTheOptOutCountOnce(t *testing.T) {
	_, filter, cs, p := transitionWatcher(t, nil)
	optOut := map[string]string{CollectAnnotation: "false"}

	setAnnotations(t, cs, p, optOut)
	waitFor(t, 5*time.Second, "the opt-out to be counted",
		func() bool { return filter.Snapshot().ExcludedPodAnnotation == 1 })

	for range 5 {
		setAnnotations(t, cs, p, optOut)
	}
	time.Sleep(200 * time.Millisecond)

	if c := filter.Snapshot(); c.ExcludedPodAnnotation != 1 {
		t.Errorf("excluded_pod_annotation = %d after five more updates, want 1", c.ExcludedPodAnnotation)
	}
}

// Removing the annotation is a change too, and the pod returns to the collected
// set. A backend that saw it leave has to be able to see it come back.
func TestRemovingTheAnnotationCountsThePodObservedAgain(t *testing.T) {
	watcher, filter, cs, p := transitionWatcher(t, nil)

	setAnnotations(t, cs, p, map[string]string{CollectAnnotation: "false"})
	waitFor(t, 5*time.Second, "the opt-out to be counted",
		func() bool { return filter.Snapshot().ExcludedPodAnnotation == 1 })

	setAnnotations(t, cs, p, nil)
	waitFor(t, 5*time.Second, "the pod to be counted observed again",
		func() bool { return filter.Snapshot().PodsObserved == 2 })

	if pods := watcher.Pods(); len(pods) != 1 {
		t.Errorf("collected pods = %d, want the pod back", len(pods))
	}
}

// The narrower control moves its own counter on the same terms. The pod stays
// collected throughout, so nothing but the profiling count may change.
func TestRefusingTheProfilerOnARunningPodIsCounted(t *testing.T) {
	watcher, filter, cs, p := transitionWatcher(t, nil)

	setAnnotations(t, cs, p, map[string]string{ProfileAnnotation: "false"})
	waitFor(t, 5*time.Second, "the profiling opt-out to be counted",
		func() bool { return filter.Snapshot().ExcludedProfilingAnnotation == 1 })

	if c := filter.Snapshot(); c.ExcludedPodAnnotation != 0 {
		t.Errorf("excluded_pod_annotation = %d, want 0: the pod refused the profiler, not collection",
			c.ExcludedPodAnnotation)
	}
	if pods := watcher.Pods(); len(pods) != 1 {
		t.Errorf("collected pods = %d, want the pod to stay collected", len(pods))
	}
	if cs := watcher.ContainersOnNode("node-1"); len(cs) != 0 {
		t.Errorf("targets answer names %+v, want nothing for a pod that refused the profiler", cs)
	}
}

// A pod that was already excluded when it appeared was counted then. The
// updates that follow carry the same answer and must not count it twice.
func TestAPodExcludedOnArrivalIsNotCountedAgain(t *testing.T) {
	_, filter, cs, p := transitionWatcher(t, map[string]string{CollectAnnotation: "false"})
	if c := filter.Snapshot(); c.ExcludedPodAnnotation != 1 || c.PodsObserved != 0 {
		t.Fatalf("coverage on arrival = %+v, want 1 excluded and 0 observed", c)
	}

	for range 5 {
		setAnnotations(t, cs, p, map[string]string{CollectAnnotation: "false"})
	}
	time.Sleep(200 * time.Millisecond)

	if c := filter.Snapshot(); c.ExcludedPodAnnotation != 1 || c.PodsObserved != 0 {
		t.Errorf("coverage = %+v, want the arrival's single count and nothing since", c)
	}
}
