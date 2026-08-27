package collector

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/utils/ptr"
)

// counterWatcher starts a PodWatcher over the given pods and returns the
// clientset for follow-up updates. Unlike the restart-journal helper it hands
// back the watcher itself: what these tests read is a snapshot method, not an
// event stream.
func counterWatcher(t *testing.T, filter *Filter, initial ...*corev1.Pod) (*fake.Clientset, *PodWatcher) {
	t.Helper()
	objects := make([]runtime.Object, 0, len(initial))
	for _, p := range initial {
		objects = append(objects, p)
	}
	clientset := fake.NewClientset(objects...)

	watcher := NewPodWatcher(clientset, func(PodInfo) {})
	if filter != nil {
		watcher.SetFilter(filter)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("watcher returned error: %v", err)
		}
	})
	return clientset, watcher
}

// awaitCounters polls until the reading satisfies want, which is how these
// tests avoid racing the informer: the index and the counter baselines fill on
// the informer goroutine, and a single read could catch either half-built.
func awaitCounters(t *testing.T, w *PodWatcher, want func([]RestartCounter) bool, what string) []RestartCounter {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last []RestartCounter
	for time.Now().Before(deadline) {
		last = w.RestartCounters()
		if want(last) {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; last reading = %+v", what, last)
	return nil
}

func counterFor(records []RestartCounter, pod, container string) (RestartCounter, bool) {
	for _, r := range records {
		if r.Pod == pod && r.Container == container {
			return r, true
		}
	}
	return RestartCounter{}, false
}

// The point of the payload: a container whose counter already stood at 40 when
// the agent arrived reports all 40, and reports that it watched none of them.
// The restart journal cannot say either — it baselines the count away, because
// those restarts have no instants to be placed at (ADR 0020 §5, ADR 0034).
func TestCounterCarriesTheHistoryTheWindowsCannot(t *testing.T) {
	createdAt := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	crashedAt := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.CreationTimestamp = metav1.NewTime(createdAt)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:                 "app",
		RestartCount:         40,
		LastTerminationState: terminated("OOMKilled", 137, crashedAt),
		State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(crashedAt.Add(time.Second))}},
	}}
	_, watcher := counterWatcher(t, nil, p)

	records := awaitCounters(t, watcher, func(rs []RestartCounter) bool {
		_, ok := counterFor(rs, "checkout-1", "app")
		return ok
	}, "the first reading")

	got, _ := counterFor(records, "checkout-1", "app")
	if got.Restarts != 40 {
		t.Errorf("restarts = %d, want 40 — the counter as the kubelet keeps it", got.Restarts)
	}
	if got.RestartsBeforeObservation != 40 {
		t.Errorf("restarts_before_observation = %d, want 40 — none of them were watched",
			got.RestartsBeforeObservation)
	}
	if got.ObservedSince.IsZero() {
		t.Error("observed_since is zero; without it the 40 restarts have no interval")
	}
	if !got.PodCreatedAt.Equal(createdAt) {
		t.Errorf("pod_created_at = %s, want %s — it is the other end of that interval",
			got.PodCreatedAt, createdAt)
	}
	if got.ContainerStartedAt == nil {
		t.Error("container_started_at is absent for a running container; it is how long this one has survived")
	}
	if got.LastTermination == nil || got.LastTermination.Reason != "OOMKilled" ||
		got.LastTermination.ExitCode != 137 || !got.LastTermination.FinishedAt.Equal(crashedAt) {
		t.Errorf("last_termination = %+v, want OOMKilled/137 at %s — the one restart Kubernetes dates",
			got.LastTermination, crashedAt)
	}
	if got.Workload != (WorkloadRef{Kind: "StatefulSet", Name: "checkout"}) {
		t.Errorf("workload = %+v, want StatefulSet/checkout", got.Workload)
	}
}

// A later reading grows; what the agent did not watch does not. The pair is
// what lets a consumer split a total into "before us" and "ours" without
// subtracting one payload from another.
func TestObservedShareGrowsAndTheUnobservedPartDoesNot(t *testing.T) {
	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 40}}
	clientset, watcher := counterWatcher(t, nil, p)

	awaitCounters(t, watcher, func(rs []RestartCounter) bool {
		_, ok := counterFor(rs, "checkout-1", "app")
		return ok
	}, "the first reading")

	advance := p.DeepCopy()
	advance.Status.ContainerStatuses[0].RestartCount = 43
	if _, err := clientset.CoreV1().Pods(advance.Namespace).Update(t.Context(), advance, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating pod: %v", err)
	}

	records := awaitCounters(t, watcher, func(rs []RestartCounter) bool {
		r, ok := counterFor(rs, "checkout-1", "app")
		return ok && r.Restarts == 43
	}, "the advanced reading")

	got, _ := counterFor(records, "checkout-1", "app")
	if got.RestartsBeforeObservation != 40 {
		t.Errorf("restarts_before_observation = %d, want 40 — the three new ones were watched, so they are in the windows",
			got.RestartsBeforeObservation)
	}
}

// A counter below the baseline is a different container wearing a name the
// agent has seen — a pod recreated under the same name, whose history is its
// own. Carrying the old baseline forward would report restarts that belong to a
// pod that no longer exists.
func TestACounterThatWentBackwardsRebaselinesWhole(t *testing.T) {
	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 40}}
	clientset, watcher := counterWatcher(t, nil, p)

	awaitCounters(t, watcher, func(rs []RestartCounter) bool {
		r, ok := counterFor(rs, "checkout-1", "app")
		return ok && r.RestartsBeforeObservation == 40
	}, "the first reading")

	// The StatefulSet replaced the pod: same name, fresh counter.
	recreated := p.DeepCopy()
	recreated.Status.ContainerStatuses[0].RestartCount = 2
	if _, err := clientset.CoreV1().Pods(recreated.Namespace).Update(t.Context(), recreated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating pod: %v", err)
	}

	records := awaitCounters(t, watcher, func(rs []RestartCounter) bool {
		r, ok := counterFor(rs, "checkout-1", "app")
		return ok && r.Restarts == 2
	}, "the rebaselined reading")

	got, _ := counterFor(records, "checkout-1", "app")
	if got.RestartsBeforeObservation != 2 {
		t.Errorf("restarts_before_observation = %d, want 2 — the old pod's 40 are not this pod's history",
			got.RestartsBeforeObservation)
	}
}

// A container that has never restarted has nothing to report. The payload is a
// snapshot, so its absence says the counter is zero — and saying it with a
// record per container in the cluster would be the same claim at a hundred
// times the size.
func TestContainersThatNeverRestartedHaveNoRecord(t *testing.T) {
	quiet := pod("quiet-1", ptr.To(controllerRef("Deployment", "quiet")))
	quiet.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 0}}
	noisy := pod("noisy-1", ptr.To(controllerRef("Deployment", "noisy")))
	noisy.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 3}}
	_, watcher := counterWatcher(t, nil, quiet, noisy)

	records := awaitCounters(t, watcher, func(rs []RestartCounter) bool {
		_, ok := counterFor(rs, "noisy-1", "app")
		return ok
	}, "the noisy pod's reading")

	if _, ok := counterFor(records, "quiet-1", "app"); ok {
		t.Error("a container that never restarted produced a record")
	}
}

// Init containers restart too, and a failing init container is the reason a pod
// never starts. The journal counts them; so does the reading.
func TestInitContainersAreCounted(t *testing.T) {
	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init-db", RestartCount: 7}}
	_, watcher := counterWatcher(t, nil, p)

	records := awaitCounters(t, watcher, func(rs []RestartCounter) bool {
		_, ok := counterFor(rs, "checkout-1", "init-db")
		return ok
	}, "the init container's reading")

	got, _ := counterFor(records, "checkout-1", "init-db")
	if got.Restarts != 7 {
		t.Errorf("restarts = %d, want 7", got.Restarts)
	}
}

// A pod excluded by the filters is not in the admitted index, and the reading
// is assembled from that index — so an excluded pod cannot appear here even
// though its counter is visible in the same cache the reading walks.
func TestExcludedPodsHaveNoReading(t *testing.T) {
	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 9}}
	other := pod("visible-1", ptr.To(controllerRef("Deployment", "visible")))
	other.Namespace = "other"
	other.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 4}}

	// "shop" is denied; "other" is not.
	_, watcher := counterWatcher(t, NewFilter(nil, []string{"shop"}), p, other)

	records := awaitCounters(t, watcher, func(rs []RestartCounter) bool {
		_, ok := counterFor(rs, "visible-1", "app")
		return ok
	}, "the admitted pod's reading")

	if _, ok := counterFor(records, "checkout-1", "app"); ok {
		t.Error("an excluded pod produced a restart reading")
	}
}

// stubPodLister serves a fixed pod list, so a test can hold the informer's
// shared store at a chosen moment instead of racing it.
type stubPodLister struct {
	corelisters.PodLister
	pods []*corev1.Pod
}

func (s stubPodLister) List(labels.Selector) ([]*corev1.Pod, error) { return s.pods, nil }

// TestTheReadingNeverClaimsMoreHistoryThanItCounts builds the interleaving by
// hand instead of waiting for it (ADR 0043).
//
// client-go updates the shared store before dispatching to the handler that
// maintains the baseline, so for one dispatch the store holds a counter the
// baseline has not seen. A StatefulSet replacing a pod under the same name makes
// it visible: store 2, baseline 40, and the subtraction ADR 0034 §2 prescribes
// returns a negative number.
func TestTheReadingNeverClaimsMoreHistoryThanItCounts(t *testing.T) {
	w := NewPodWatcher(fake.NewClientset(), func(PodInfo) {})

	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 40}}

	// The agent has seen the old incarnation: baseline 40.
	w.reportRestarts(p)

	// Make the pod resolvable without running an informer.
	w.index[p.UID] = podIndexEntry{
		namespace: p.Namespace,
		name:      p.Name,
		workload:  WorkloadRef{Kind: "StatefulSet", Name: "checkout"},
	}

	// The store now holds the replacement — same name, fresh counter — and the
	// handler that would rebaseline has not run yet.
	recreated := p.DeepCopy()
	recreated.Status.ContainerStatuses[0].RestartCount = 2
	w.podLister = stubPodLister{pods: []*corev1.Pod{recreated}}

	got, ok := counterFor(w.RestartCounters(), "checkout-1", "app")
	if !ok {
		t.Fatal("no reading for the recreated pod")
	}
	if got.RestartsBeforeObservation > got.Restarts {
		t.Errorf("reading claims %d restarts predate observation of a counter at %d; "+
			"ADR 0034 tells a consumer to subtract these and they would get %d",
			got.RestartsBeforeObservation, got.Restarts,
			got.Restarts-got.RestartsBeforeObservation)
	}

	// And once the handler catches up, the reading is the new incarnation's.
	w.reportRestarts(recreated)
	got, ok = counterFor(w.RestartCounters(), "checkout-1", "app")
	if !ok {
		t.Fatal("no reading after the rebaseline")
	}
	if got.Restarts != 2 || got.RestartsBeforeObservation != 2 {
		t.Errorf("after the rebaseline: restarts=%d before_observation=%d, want 2 and 2 — "+
			"the old pod's history is not this pod's", got.Restarts, got.RestartsBeforeObservation)
	}
}
