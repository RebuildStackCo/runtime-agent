package collector

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

// restartWatcher starts a PodWatcher over the given pod and returns the restart
// events channel plus the started clientset for follow-up updates.
//
// It does not return until a pod *created after the watcher started* has been
// delivered. That is the barrier these tests need: a cache sync only proves the
// initial List landed, while an update is delivered over the reflector's watch,
// which is established afterwards. Without the barrier every update raced that
// call and was silently dropped by the fake clientset, which has no replay.
func restartWatcher(t *testing.T, initial *corev1.Pod) (*fake.Clientset, chan model.ContainerRestart) {
	t.Helper()
	clientset := fake.NewClientset(initial)

	events := make(chan model.ContainerRestart, 16)
	observed := make(chan string, 16)
	watcher := NewPodWatcher(clientset, func(p model.PodInfo) { observed <- p.Name })
	watcher.OnContainerRestart(func(r model.ContainerRestart) { events <- r })

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
	return clientset, events
}

// waitForWatch creates a throwaway pod and blocks until the watcher reports it.
// A pod that did not exist at List time can only arrive over the watch, so its
// delivery proves the watch is live. It is retried because the create itself
// can land before the watch and be lost the same way.
func waitForWatch(t *testing.T, clientset *fake.Clientset, observed chan string) {
	t.Helper()
	const probeName = "watch-probe"
	deadline := time.Now().Add(10 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		probe := pod(fmt.Sprintf("%s-%d", probeName, attempt), nil)
		if _, err := clientset.CoreV1().Pods(probe.Namespace).Create(t.Context(), probe, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating watch probe: %v", err)
		}
		if observedName(observed, probeName, 250*time.Millisecond) {
			return
		}
		// The probe was lost to the same race; try another one.
	}
	t.Fatal("the pod watch never delivered a probe pod")
}

// observedName drains reported pod names until one has the given prefix or the
// timeout expires.
func observedName(observed chan string, prefix string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case name := <-observed:
			if strings.HasPrefix(name, prefix) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func waitRestart(t *testing.T, events chan model.ContainerRestart) model.ContainerRestart {
	t.Helper()
	select {
	case r := <-events:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no container restart reported before timeout")
		return model.ContainerRestart{}
	}
}

// updatePod pushes a status update and fails the test if it does not land.
func updatePod(t *testing.T, clientset *fake.Clientset, p *corev1.Pod) {
	t.Helper()
	if _, err := clientset.CoreV1().Pods(p.Namespace).Update(t.Context(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating pod: %v", err)
	}
}

func TestReportsRestartCounterAdvance(t *testing.T) {
	crashedAt := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 0}}
	clientset, events := restartWatcher(t, p)

	update := p.DeepCopy()
	update.Status.ContainerStatuses[0].RestartCount = 1
	update.Status.ContainerStatuses[0].LastTerminationState = terminated("Error", 1, crashedAt)
	updatePod(t, clientset, update)

	got := waitRestart(t, events)
	if got.Namespace != "shop" || got.Pod != "checkout-1" || got.Container != "app" {
		t.Errorf("event = %s/%s container %s, want shop/checkout-1 container app", got.Namespace, got.Pod, got.Container)
	}
	if got.Restarts != 1 {
		t.Errorf("restarts = %d, want 1", got.Restarts)
	}
	if got.Reason != "Error" {
		t.Errorf("reason = %q, want Error", got.Reason)
	}
	if got.ExitCode == nil || *got.ExitCode != 1 {
		t.Errorf("exit code = %v, want 1", got.ExitCode)
	}
	if got.Workload != (model.WorkloadRef{Kind: "StatefulSet", Name: "checkout"}) {
		t.Errorf("workload = %+v, want StatefulSet/checkout", got.Workload)
	}
	if got.ObservedAt.IsZero() {
		t.Error("observed-at is zero; it is what places the restarts in a window")
	}
}

// A container the agent has never seen may already have restarted many times.
// Those restarts happened at moments Kubernetes does not record, so reporting
// them would place a history in whichever window happens to be open now.
func TestFirstObservationBaselinesWithoutReporting(t *testing.T) {
	crashedAt := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:                 "app",
		RestartCount:         40,
		LastTerminationState: terminated("Error", 1, crashedAt),
	}}
	clientset, events := restartWatcher(t, p)

	// Nothing for the 40 restarts that predate the agent — but the next one is
	// reported as exactly one, proving the baseline was taken rather than the
	// container ignored.
	update := p.DeepCopy()
	update.Status.ContainerStatuses[0].RestartCount = 41
	updatePod(t, clientset, update)

	got := waitRestart(t, events)
	if got.Restarts != 1 {
		t.Errorf("restarts = %d, want 1 — the 40 pre-existing restarts must not be reported", got.Restarts)
	}
}

// The counter is exact even when the informer misses intermediate states: a
// jump of three is three restarts, not one.
func TestCounterAdvanceOfSeveralIsReportedInFull(t *testing.T) {
	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 2}}
	clientset, events := restartWatcher(t, p)

	update := p.DeepCopy()
	update.Status.ContainerStatuses[0].RestartCount = 5
	update.Status.ContainerStatuses[0].LastTerminationState = terminated("OOMKilled", 137, time.Now())
	updatePod(t, clientset, update)

	got := waitRestart(t, events)
	if got.Restarts != 3 {
		t.Errorf("restarts = %d, want 3", got.Restarts)
	}
	if got.Reason != "OOMKilled" {
		t.Errorf("reason = %q, want OOMKilled — the reason of the most recent one", got.Reason)
	}
}

// A status update that does not advance the counter is not a restart, however
// much else about the pod changed.
func TestUnchangedCounterReportsNothing(t *testing.T) {
	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 1}}
	clientset, events := restartWatcher(t, p)

	unrelated := p.DeepCopy()
	unrelated.Labels = map[string]string{"touched": "yes"}
	unrelated.Status.Phase = corev1.PodRunning
	updatePod(t, clientset, unrelated)

	// Then a real restart. Receiving it as the *first* event proves the
	// unrelated update produced none.
	advance := unrelated.DeepCopy()
	advance.Status.ContainerStatuses[0].RestartCount = 2
	updatePod(t, clientset, advance)

	got := waitRestart(t, events)
	if got.Restarts != 1 {
		t.Errorf("first reported event = %+v, want a single restart", got)
	}
}

// Init containers restart too, and a native sidecar is declared as one — it
// runs for the pod's whole life and can crash-loop like any other container.
func TestReportsInitContainerRestarts(t *testing.T) {
	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "sidecar", RestartCount: 0}}
	clientset, events := restartWatcher(t, p)

	update := p.DeepCopy()
	update.Status.InitContainerStatuses[0].RestartCount = 1
	update.Status.InitContainerStatuses[0].LastTerminationState = terminated("Error", 2, time.Now())
	updatePod(t, clientset, update)

	got := waitRestart(t, events)
	if got.Container != "sidecar" {
		t.Errorf("container = %q, want sidecar", got.Container)
	}
}

// A container currently lying dead has its termination in state.terminated
// rather than lastState.terminated (restartPolicy Never, or the moment before
// the restart lands).
func TestReasonFallsBackToTheCurrentTerminatedState(t *testing.T) {
	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 0}}
	clientset, events := restartWatcher(t, p)

	update := p.DeepCopy()
	update.Status.ContainerStatuses[0].RestartCount = 1
	update.Status.ContainerStatuses[0].State = terminated("ContainerCannotRun", 128, time.Now())
	updatePod(t, clientset, update)

	got := waitRestart(t, events)
	if got.Reason != "ContainerCannotRun" {
		t.Errorf("reason = %q, want ContainerCannotRun", got.Reason)
	}
}

// A restart whose termination state is gone entirely still counts. Losing the
// reason must not lose the restart.
func TestRestartWithNoTerminationStateStillCounts(t *testing.T) {
	p := pod("checkout-1", ptr.To(controllerRef("StatefulSet", "checkout")))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 0}}
	clientset, events := restartWatcher(t, p)

	update := p.DeepCopy()
	update.Status.ContainerStatuses[0].RestartCount = 1
	updatePod(t, clientset, update)

	got := waitRestart(t, events)
	if got.Restarts != 1 || got.Reason != "" || got.ExitCode != nil {
		t.Errorf("event = %+v, want one restart with no reason", got)
	}
}
