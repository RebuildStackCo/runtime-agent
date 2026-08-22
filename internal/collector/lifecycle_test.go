package collector

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

// disruptionWatcher is a started PodWatcher plus the channel its disruptions
// arrive on. It uses the same watch barrier as restartWatcher — see the comment
// there for why a cache sync is not enough.
type disruptionWatcher struct {
	fake   *fake.Clientset
	events chan PodDisruption
}

func newWatcherWithDisruptions(t *testing.T, initial *corev1.Pod) disruptionWatcher {
	t.Helper()
	clientset := fake.NewClientset(initial)

	events := make(chan PodDisruption, 16)
	observed := make(chan string, 16)
	watcher := NewPodWatcher(clientset, func(p PodInfo) { observed <- p.Name })
	watcher.OnPodDisruption(func(d PodDisruption) { events <- d })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("watcher returned error: %v", err)
		}
	})
	waitForWatch(t, clientset, observed)
	return disruptionWatcher{fake: clientset, events: events}
}

func waitDisruption(t *testing.T, events chan PodDisruption) PodDisruption {
	t.Helper()
	select {
	case d := <-events:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("no pod disruption reported before timeout")
		return PodDisruption{}
	}
}

func scheduledCondition(status corev1.ConditionStatus, reason string) corev1.PodCondition {
	return corev1.PodCondition{
		Type:   corev1.PodScheduled,
		Status: status,
		Reason: reason,
	}
}

func TestUnscheduledReasonDistinguishesCapacityFromAGate(t *testing.T) {
	cases := []struct {
		name       string
		conditions []corev1.PodCondition
		want       string
	}{
		{
			"nothing fits",
			[]corev1.PodCondition{scheduledCondition(corev1.ConditionFalse, "Unschedulable")},
			"Unschedulable",
		},
		{
			// A gated pod is held on purpose. Reporting it as unschedulable
			// would invent a capacity shortage that does not exist.
			"deliberately gated",
			[]corev1.PodCondition{scheduledCondition(corev1.ConditionFalse, "SchedulingGated")},
			"SchedulingGated",
		},
		{
			"scheduler failed on the spec",
			[]corev1.PodCondition{scheduledCondition(corev1.ConditionFalse, "SchedulerError")},
			"SchedulerError",
		},
		{
			"an upstream reason we do not know",
			[]corev1.PodCondition{scheduledCondition(corev1.ConditionFalse, "SomeFutureReason")},
			OtherReason,
		},
		{
			"no reason given at all",
			[]corev1.PodCondition{scheduledCondition(corev1.ConditionFalse, "")},
			OtherReason,
		},
		{
			"scheduled",
			[]corev1.PodCondition{scheduledCondition(corev1.ConditionTrue, "")},
			"",
		},
		{
			// Not yet through the scheduler is a moment, not a state.
			"no PodScheduled condition yet",
			nil,
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pod("checkout-1", nil)
			p.Status.Conditions = tc.conditions
			if got := unscheduledReason(p); got != tc.want {
				t.Errorf("unscheduledReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDisruptionOfPrefersTheConditionAndItsTimestamp(t *testing.T) {
	at := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	p := pod("checkout-1", nil)
	p.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.DisruptionTarget,
		Status:             corev1.ConditionTrue,
		Reason:             "PreemptionByScheduler",
		LastTransitionTime: metav1.NewTime(at),
	}}

	reason, got, ok := disruptionOf(p)
	if !ok {
		t.Fatal("a pod with DisruptionTarget=True must be reported as disrupted")
	}
	if reason != "PreemptionByScheduler" {
		t.Errorf("reason = %q, want PreemptionByScheduler", reason)
	}
	if !got.Equal(at) {
		t.Errorf("disrupted at %s, want %s — the cluster's own timestamp, not the observation", got, at)
	}
}

// The kubelet's older signal still appears on node-pressure evictions, and a
// pod carrying only that must not be missed.
func TestDisruptionOfFallsBackToTheEvictedPhase(t *testing.T) {
	p := pod("checkout-1", nil)
	p.Status.Phase = corev1.PodFailed
	p.Status.Reason = "Evicted"

	reason, at, ok := disruptionOf(p)
	if !ok || reason != "Evicted" {
		t.Fatalf("reason = %q, ok = %v; want Evicted, true", reason, ok)
	}
	if at.IsZero() {
		t.Error("a fallback disruption still needs a timestamp to be placed in a window")
	}
}

// A DisruptionTarget condition that is present but False describes a pod that is
// *not* being disrupted.
func TestDisruptionOfIgnoresAFalseCondition(t *testing.T) {
	p := pod("checkout-1", nil)
	p.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.DisruptionTarget,
		Status: corev1.ConditionFalse,
		Reason: "PreemptionByScheduler",
	}}
	if _, _, ok := disruptionOf(p); ok {
		t.Error("DisruptionTarget=False must not be reported as a disruption")
	}
}

func TestDisruptionOfIgnoresAHealthyPod(t *testing.T) {
	p := pod("checkout-1", nil)
	p.Status.Phase = corev1.PodRunning
	if _, _, ok := disruptionOf(p); ok {
		t.Error("a running pod must not be reported as disrupted")
	}
}

// A pod that failed for its own reasons — its container exited non-zero — is
// not a disruption. The cluster did not remove it.
func TestDisruptionOfIgnoresAnOrdinaryFailure(t *testing.T) {
	p := pod("checkout-1", nil)
	p.Status.Phase = corev1.PodFailed
	p.Status.Reason = "Error"
	if _, _, ok := disruptionOf(p); ok {
		t.Error("a pod that failed on its own must not be reported as disrupted")
	}
}

// Disruptions reach the watcher exactly once, however many status updates the
// condemned pod receives before it disappears.
func TestReportsPodDisruptionOnce(t *testing.T) {
	at := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	p := pod("checkout-1", ptr.To(controllerRef("Deployment", "checkout")))
	clientset := newWatcherWithDisruptions(t, p)

	condemned := p.DeepCopy()
	condemned.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.DisruptionTarget,
		Status:             corev1.ConditionTrue,
		Reason:             "TerminationByKubelet",
		LastTransitionTime: metav1.NewTime(at),
	}}
	updatePod(t, clientset.fake, condemned)

	got := waitDisruption(t, clientset.events)
	if got.Pod != "checkout-1" || got.Reason != "TerminationByKubelet" {
		t.Errorf("event = %+v, want checkout-1 terminated by the kubelet", got)
	}
	if got.Node != "node-1" {
		t.Errorf("node = %q, want node-1 — the node under pressure is the point", got.Node)
	}
	if got.Workload != (WorkloadRef{Kind: "Deployment", Name: "checkout"}) {
		t.Errorf("workload = %+v, want Deployment/checkout", got.Workload)
	}

	// Another status update carrying the same condition must not re-report.
	// Seeing the *second* pod's event next proves this one produced none.
	again := condemned.DeepCopy()
	again.Status.Phase = corev1.PodFailed
	updatePod(t, clientset.fake, again)

	second := pod("checkout-2", ptr.To(controllerRef("Deployment", "checkout")))
	second.Status.Conditions = condemned.Status.Conditions
	if _, err := clientset.fake.CoreV1().Pods(second.Namespace).Create(t.Context(), second, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the second pod: %v", err)
	}

	next := waitDisruption(t, clientset.events)
	if next.Pod != "checkout-2" {
		t.Errorf("next event is for %q, want checkout-2 (the first pod was reported twice)", next.Pod)
	}
}
