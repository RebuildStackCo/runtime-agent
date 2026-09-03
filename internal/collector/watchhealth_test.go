package collector

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// The arithmetic first: what counts as "failing", given that client-go offers a
// failure callback and no matching success one.

// A single dropped connection is not an outage. The streak is measured between
// failures, so one of them spans nothing.
func TestOneFailureIsNotAnOutage(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	h := newWatchHealth(2 * time.Minute)
	h.record(t0, errors.New("connection reset"))

	if d, _ := h.failingFor(t0.Add(time.Second)); d != 0 {
		t.Errorf("failing for %s after a single error, want 0", d)
	}
	if !h.failedWithin(t0.Add(time.Second), time.Minute) {
		t.Error("the failure was not recorded at all")
	}
}

// The span counts only time the agent watched the cache fail — never the
// interval since the last failure. Measuring up to now would let one error
// grow into an outage just by waiting.
func TestTheStreakSpansTheFailuresAndNotTheSilenceAfterThem(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	h := newWatchHealth(2 * time.Minute)
	h.record(t0, errors.New("forbidden"))
	h.record(t0.Add(time.Minute), errors.New("forbidden"))
	h.record(t0.Add(3*time.Minute), errors.New("forbidden"))

	if d, _ := h.failingFor(t0.Add(3 * time.Minute)); d != 3*time.Minute {
		t.Errorf("streak = %s, want the 3m between the first failure and the last", d)
	}
	// Nothing more arrives. The streak is over, not still growing.
	if d, _ := h.failingFor(t0.Add(10 * time.Minute)); d != 0 {
		t.Errorf("streak = %s seven minutes after the last failure, want 0", d)
	}
}

// Two minutes of quiet is the reflector's own definition of a healthy API
// server, and it is what separates one outage from the next.
func TestAQuietStretchStartsANewStreakInsteadOfExtendingTheOld(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	h := newWatchHealth(2 * time.Minute)
	h.record(t0, errors.New("forbidden"))
	h.record(t0.Add(4*time.Minute), errors.New("forbidden"))

	if d, _ := h.failingFor(t0.Add(4 * time.Minute)); d != 0 {
		t.Errorf("streak = %s, want 0: the four-minute gap ended the first run", d)
	}
	h.record(t0.Add(5*time.Minute), errors.New("forbidden"))
	if d, _ := h.failingFor(t0.Add(5 * time.Minute)); d != time.Minute {
		t.Errorf("streak = %s, want the 1m of the second run only", d)
	}
}

// The watchdog waits out the limit and then names what failed. A customer
// reading the log has to learn which grant to look at.
func TestTheWatchdogStopsOnlyAfterTheStreakOutlivesTheLimitAndNamesTheResource(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := t0
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	h := newWatchHealth(2 * time.Minute)
	gating := map[string]*watchHealth{"pods": h}
	limits := watchLimits{
		streakGap:     2 * time.Minute,
		fatalStreak:   5 * time.Minute,
		checkInterval: 5 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watchdog(ctx, clock, limits, gating) }()

	// Five failures a minute apart span four minutes — the streak runs from the
	// first to the last, so it is not yet enough.
	for i := 1; i <= 5; i++ {
		advance(time.Minute)
		h.record(clock(), apierrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "", errors.New("no permission")))
	}
	if d, _ := h.failingFor(clock()); d != 4*time.Minute {
		t.Fatalf("streak = %s, want 4m", d)
	}
	select {
	case err := <-done:
		t.Fatalf("stopped after four minutes of failure: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	advance(time.Minute)
	h.record(clock(), apierrors.NewForbidden(
		schema.GroupResource{Resource: "pods"}, "", errors.New("no permission")))

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the watchdog returned no error after five minutes of failure")
		}
		if !strings.Contains(err.Error(), "pods") {
			t.Errorf("error does not name the resource: %v", err)
		}
		if !strings.Contains(err.Error(), "5m0s") {
			t.Errorf("error does not state how long it failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the watchdog did not stop after the streak outlived the limit")
	}
}

// A recovered cache is not stopped for having failed earlier: the streak has to
// be live, not merely to have happened.
func TestTheWatchdogIgnoresAStreakThatEnded(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	h := newWatchHealth(2 * time.Minute)
	for i := range 10 {
		h.record(t0.Add(time.Duration(i)*time.Minute), errors.New("forbidden"))
	}
	// Ten minutes of failure, then quiet: the watch is being served again.
	now := t0.Add(30 * time.Minute)
	limits := watchLimits{streakGap: 2 * time.Minute, fatalStreak: 5 * time.Minute, checkInterval: 5 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := watchdog(ctx, func() time.Time { return now }, limits, map[string]*watchHealth{"pods": h}); err != nil {
		t.Fatalf("stopped over a streak that already ended: %v", err)
	}
}

// Now the wiring. Every cache the collected view is assembled from must be
// tracked — a handler registered on six of eight informers is a hole shaped
// exactly like the one this change closes.
func TestEveryGatingCacheIsTracked(t *testing.T) {
	w := NewPodWatcher(fake.NewClientset(), func(model.PodInfo) {})
	want := []string{
		"cronjobs", "daemonsets", "deployments", "jobs",
		"namespaces", "pods", "replicasets", "statefulsets",
	}
	if len(w.gating) != len(want) {
		t.Errorf("tracked %d gating caches, want %d: %v", len(w.gating), len(want), w.gating)
	}
	for _, name := range want {
		if w.gating[name] == nil {
			t.Errorf("the %s cache is not tracked", name)
		}
	}
	for _, source := range w.policySources {
		if source.health == nil {
			t.Errorf("the %s policy source is not tracked", source.name)
		}
	}
}

// The headline for the gating half: a refusal the agent cannot collect around
// stops it. Before this change the pod cache never synced, WaitForCacheSync had
// no timeout, and the agent sat there holding nothing and reporting nothing.
func TestASustainedRefusalStopsTheAgentInsteadOfWaitingForever(t *testing.T) {
	clientset := fake.NewClientset()
	forbidden := func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "", errors.New("no permission"))
	}
	clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "", errors.New("no permission"))
	})
	clientset.PrependWatchReactor("pods", forbidden)

	w := NewPodWatcher(clientset, func(model.PodInfo) {})
	w.SetFilter(NewFilter(nil, nil))
	// Real clock, compressed limits: the reflector's own retries supply the
	// streak, so what is under test is the wiring and not a stubbed record.
	w.useWatchLimits(watchLimits{
		streakGap:      10 * time.Second,
		fatalStreak:    2 * time.Second,
		unavailableFor: 3 * time.Minute,
		checkInterval:  20 * time.Millisecond,
	}, time.Now)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := w.Run(ctx)
	if err == nil {
		t.Fatal("the watcher returned no error while the pod cache was refused")
	}
	if !strings.Contains(err.Error(), "pods") {
		t.Errorf("error does not name the refused resource: %v", err)
	}
}

// The other half, and the one ADR 0033 §5 recorded as unfixed: a source that
// filled and then lost its watch. HasSynced still says yes and the store still
// answers, so without the health record the payload would declare completeness
// over a frozen cache.
func TestAPolicySourceThatFilledAndThenWentDarkIsDeclaredUnavailable(t *testing.T) {
	clientset := fake.NewClientset(
		policyPod("web-1", "web"),
		replicaSetOwnedBy("web-abc", "web"),
		budget(),
	)
	feed := newStoppableWatch()
	var mu sync.Mutex
	var revoked bool
	clientset.PrependWatchReactor("poddisruptionbudgets", func(k8stesting.Action) (bool, watch.Interface, error) {
		mu.Lock()
		defer mu.Unlock()
		if revoked {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "policy", Resource: "poddisruptionbudgets"},
				"", errors.New("no permission"))
		}
		return true, feed, nil
	})

	w := NewPodWatcher(clientset, func(model.PodInfo) {})
	w.SetFilter(NewFilter(nil, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// The permission is intact: the cache fills and the payload declares nothing.
	waitFor(t, 5*time.Second, "the budget cache to fill", func() bool {
		_, unavailable := w.WorkloadPolicies()
		return len(w.Pods()) > 0 && len(unavailable) == 0
	})

	// Now it is taken away from the running agent. The store keeps its contents;
	// only the feed stops.
	mu.Lock()
	revoked = true
	mu.Unlock()
	feed.Stop()

	waitFor(t, 10*time.Second, "the revoked source to be declared", func() bool {
		_, unavailable := w.WorkloadPolicies()
		return len(unavailable) == 1 && unavailable[0] == "pod_disruption_budgets"
	})

	// The revocation touched one payload and nothing else: the pods are still
	// collected, and the agent is still running (ADR 0033 §1).
	if len(w.Pods()) == 0 {
		t.Error("the revoked policy source stopped unrelated collection")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the watcher stopped over a policy source: %v", err)
	}
}

// A cluster policy payload asks about its own sources, so the revocation of a
// workload-policy source must not appear in it.
func TestAFailingSourceIsDeclaredOnlyByThePayloadThatReadsIt(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	synced := func() bool { return true }
	failing := newWatchHealth(2 * time.Minute)
	failing.record(now.Add(-time.Minute), errors.New("forbidden"))

	w := &PodWatcher{
		now:    func() time.Time { return now },
		limits: defaultWatchLimits(),
		policySources: []policySource{
			{name: "pod_disruption_budgets", synced: synced, health: failing},
			{name: "horizontal_pod_autoscalers", synced: synced, health: newWatchHealth(time.Minute)},
			{name: "persistent_volume_claims", synced: synced, health: newWatchHealth(time.Minute)},
			{name: "limit_ranges", synced: synced, health: newWatchHealth(time.Minute)},
			{name: "resource_quotas", synced: synced, health: newWatchHealth(time.Minute)},
			{name: "priority_classes", synced: synced, health: newWatchHealth(time.Minute)},
			{name: "storage_classes", synced: synced, health: newWatchHealth(time.Minute)},
		},
	}
	workload := w.unavailablePolicySources(workloadPolicySources...)
	if len(workload) != 1 || workload[0] != "pod_disruption_budgets" {
		t.Errorf("workload payload sources = %v, want the failing one", workload)
	}
	if cluster := w.unavailablePolicySources(clusterPolicySources...); len(cluster) != 0 {
		t.Errorf("cluster payload sources = %v, want none: it does not read budgets", cluster)
	}
}

// A failure old enough to have healed is not carried into later captures: the
// field is about this capture and no other (ADR 0033 §4).
func TestAnOldFailureIsNotCarriedIntoLaterCaptures(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	h := newWatchHealth(2 * time.Minute)
	h.record(now.Add(-time.Hour), errors.New("forbidden"))
	w := &PodWatcher{
		now:    func() time.Time { return now },
		limits: defaultWatchLimits(),
		policySources: []policySource{
			{name: "pod_disruption_budgets", synced: func() bool { return true }, health: h},
		},
	}
	if got := w.unavailablePolicySources("pod_disruption_budgets"); len(got) != 0 {
		t.Errorf("unavailable = %v, want none: the failure is an hour old", got)
	}
}

// stoppableWatch is a watch whose Stop is idempotent, so a test may end the
// feed without racing the reflector's own shutdown.
type stoppableWatch struct {
	ch   chan watch.Event
	once sync.Once
}

func newStoppableWatch() *stoppableWatch {
	return &stoppableWatch{ch: make(chan watch.Event)}
}

func (s *stoppableWatch) Stop() { s.once.Do(func() { close(s.ch) }) }

func (s *stoppableWatch) ResultChan() <-chan watch.Event { return s.ch }

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
