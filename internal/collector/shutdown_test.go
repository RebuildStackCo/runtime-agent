package collector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

// Handlers are registered after the caches sync, and the informers stop the
// moment the run context is canceled — so a shutdown landing between the two
// has its registration refused. Reporting that as an error takes the whole
// controller down with exit 1 on an ordinary SIGTERM, and it is what made
// TestCollectsContainersAndImages flake: that test cancels as soon as it has
// seen its pods, which is exactly this window.

func TestPodWatcherShutdownBeforeRegistrationIsNotAFailure(t *testing.T) {
	watcher := NewPodWatcher(fake.NewClientset(pod("checkout-1", nil)), func(model.PodInfo) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watcher.afterSync = func(informer cache.SharedIndexInformer) {
		cancel()
		waitUntilStopped(t, informer)
	}

	if err := watcher.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil: a canceled context is a shutdown, not a watch failure", err)
	}
}

func TestNodeWatcherShutdownBeforeRegistrationIsNotAFailure(t *testing.T) {
	watcher := NewNodeWatcher(fake.NewClientset(node("node-a", nil)), func(model.NodeInfo) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watcher.afterSync = func(informer cache.SharedIndexInformer) {
		cancel()
		waitUntilStopped(t, informer)
	}

	if err := watcher.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil: a canceled context is a shutdown, not a watch failure", err)
	}
}

// A watch failure still stops the agent, canceled context or not: the verdict
// is the reason the run context was canceled in the first place, and losing it
// here would turn a blind agent into a clean exit.
func TestRegistrationFailureKeepsTheWatchdogVerdict(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fatal := make(chan error, 1)
	verdict := context.DeadlineExceeded
	fatal <- verdict

	if err := registrationFailure(ctx, fatal, "pod", context.Canceled); !errors.Is(err, verdict) {
		t.Fatalf("registrationFailure = %v, want the watchdog verdict %v", err, verdict)
	}
}

// With neither a verdict nor a cancellation, a refused registration is what it
// looks like: the agent cannot watch, and must say so.
func TestRegistrationFailureReportsAGenuineError(t *testing.T) {
	err := registrationFailure(context.Background(), make(chan error, 1), "pod", context.Canceled)
	if err == nil {
		t.Fatal("registrationFailure = nil on a live context, want an error")
	}
	if want := "register pod handler"; !strings.Contains(err.Error(), want) {
		t.Fatalf("registrationFailure = %q, want it to name %q", err, want)
	}
}

// waitUntilStopped blocks until the informer refuses new handlers. IsStopped
// reads the flag AddEventHandler checks, under the same lock, so waiting on it
// is what makes the registration below deterministic rather than a race.
func waitUntilStopped(t *testing.T, informer cache.SharedIndexInformer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !informer.IsStopped() {
		if time.Now().After(deadline) {
			t.Fatal("informer still running 5s after its run context was canceled")
		}
		time.Sleep(time.Millisecond)
	}
}
