package collector

import (
	"context"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func terminated(reason string, exitCode int32, finishedAt time.Time) corev1.ContainerState {
	return corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{
			Reason:     reason,
			ExitCode:   exitCode,
			FinishedAt: metav1.NewTime(finishedAt),
		},
	}
}

// oomWatcher starts a PodWatcher over the given pod and returns the OOM
// events channel plus the started clientset for follow-up updates.
func oomWatcher(t *testing.T, initial *corev1.Pod) (*fake.Clientset, chan model.OOMKill) {
	t.Helper()
	clientset := fake.NewClientset(initial)

	events := make(chan model.OOMKill, 16)
	watcher := NewPodWatcher(clientset, func(model.PodInfo) {})
	watcher.OnOOMKill(func(o model.OOMKill) { events <- o })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("watcher returned error: %v", err)
		}
	})
	return clientset, events
}

func waitOOM(t *testing.T, events chan model.OOMKill) model.OOMKill {
	t.Helper()
	select {
	case o := <-events:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("no OOM kill reported before timeout")
		return model.OOMKill{}
	}
}

func TestReportsOOMKillOnceAcrossStatusUpdates(t *testing.T) {
	firstKill := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	clientset, events := oomWatcher(t, p)

	// The app container gets OOM-killed and restarted.
	update := p.DeepCopy()
	update.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:                 "app",
		RestartCount:         1,
		State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: terminated(oomKilledReason, 137, firstKill),
	}}
	if _, err := clientset.CoreV1().Pods("shop").Update(t.Context(), update, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating pod: %v", err)
	}

	got := waitOOM(t, events)
	if got.Namespace != "shop" || got.Pod != "checkout-1" || got.Container != "app" {
		t.Errorf("event = %s/%s container %s, want shop/checkout-1 container app", got.Namespace, got.Pod, got.Container)
	}
	if got.ExitCode != 137 || got.RestartCount != 1 || !got.FinishedAt.Equal(firstKill) {
		t.Errorf("event = %+v, want exit 137, restarts 1, finished at %s", got, firstKill)
	}
	if got.Workload != (model.WorkloadRef{Kind: "StatefulSet", Name: "checkout"}) {
		t.Errorf("workload = %+v, want StatefulSet/checkout", got.Workload)
	}
	// The app container declares a 1Gi limit (see pod() in pods_test.go).
	if got.MemoryLimitBytes == nil || *got.MemoryLimitBytes != 1<<30 {
		t.Errorf("memory limit = %v, want %d", got.MemoryLimitBytes, int64(1<<30))
	}

	// An unrelated status update carrying the same lastState must not
	// re-report; a genuinely new kill must. Receiving the second kill as
	// the *next* event proves the middle update produced none.
	unrelated := update.DeepCopy()
	unrelated.Status.Phase = corev1.PodRunning
	unrelated.Labels = map[string]string{"touched": "yes"}
	if _, err := clientset.CoreV1().Pods("shop").Update(t.Context(), unrelated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating pod: %v", err)
	}
	secondKill := firstKill.Add(time.Minute)
	again := unrelated.DeepCopy()
	again.Status.ContainerStatuses[0].RestartCount = 2
	again.Status.ContainerStatuses[0].LastTerminationState = terminated(oomKilledReason, 137, secondKill)
	if _, err := clientset.CoreV1().Pods("shop").Update(t.Context(), again, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating pod: %v", err)
	}

	next := waitOOM(t, events)
	if !next.FinishedAt.Equal(secondKill) {
		t.Errorf("next event finished at %s, want %s (the first kill was reported twice)", next.FinishedAt, secondKill)
	}
	if next.RestartCount != 2 {
		t.Errorf("next event restart count = %d, want 2", next.RestartCount)
	}
}

func TestReportsOOMKillPresentAtStart(t *testing.T) {
	// restartPolicy Never: the kill sits in state.terminated, and the pod
	// already looks like this when the watcher starts (cache replay path).
	killedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	p := pod("one-shot", nil)
	p.Status.Phase = corev1.PodFailed
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "sidecar",
		State: terminated(oomKilledReason, 137, killedAt),
	}}
	_, events := oomWatcher(t, p)

	got := waitOOM(t, events)
	if got.Pod != "one-shot" || got.Container != "sidecar" || got.ExitCode != 137 {
		t.Errorf("event = %+v, want one-shot/sidecar exit 137", got)
	}
	// The sidecar container declares no limits.
	if got.MemoryLimitBytes != nil {
		t.Errorf("memory limit = %d, want unset", *got.MemoryLimitBytes)
	}
}

func TestIgnoresNonOOMTermination(t *testing.T) {
	crashedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	p := pod("crasher", nil)
	clientset, events := oomWatcher(t, p)

	// A plain crash (reason Error) must not be reported as an OOM kill.
	update := p.DeepCopy()
	update.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:                 "app",
		RestartCount:         1,
		State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: terminated("Error", 1, crashedAt),
	}}
	if _, err := clientset.CoreV1().Pods("shop").Update(t.Context(), update, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating pod: %v", err)
	}
	// Follow with a real OOM: it must be the first event received.
	oom := update.DeepCopy()
	oom.Status.ContainerStatuses[0].RestartCount = 2
	oom.Status.ContainerStatuses[0].LastTerminationState = terminated(oomKilledReason, 137, crashedAt.Add(time.Minute))
	if _, err := clientset.CoreV1().Pods("shop").Update(t.Context(), oom, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating pod: %v", err)
	}

	got := waitOOM(t, events)
	if got.RestartCount != 2 || got.ExitCode != 137 {
		t.Errorf("event = %+v, want the OOM kill (restarts 2), not the plain crash", got)
	}
}
