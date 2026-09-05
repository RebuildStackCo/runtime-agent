package collector

import (
	"context"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

// What readiness reads (ADR 0069). It must be false before the caches fill, or a
// controller is called ready while it holds nothing and reports a cluster of no
// pods — which is the state `kubectl rollout status` used to call a success.
func TestAWatcherIsNotSyncedUntilItsCachesAre(t *testing.T) {
	pods := NewPodWatcher(fake.NewClientset(pod("checkout-1", nil)), func(model.PodInfo) {})
	nodes := NewNodeWatcher(fake.NewClientset(node("node-a", nil)), func(model.NodeInfo) {})
	if pods.Synced() || nodes.Synced() {
		t.Fatal("a watcher that has not run reports its caches synced")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// afterSync runs between the wait and handler registration, which is the
	// first instant the answer may be yes.
	podsSynced := make(chan bool, 1)
	pods.afterSync = func(cache.SharedIndexInformer) { podsSynced <- pods.Synced() }
	nodesSynced := make(chan bool, 1)
	nodes.afterSync = func(cache.SharedIndexInformer) { nodesSynced <- nodes.Synced() }

	go func() { _ = pods.Run(ctx) }()
	go func() { _ = nodes.Run(ctx) }()

	for name, ch := range map[string]chan bool{"pods": podsSynced, "nodes": nodesSynced} {
		select {
		case synced := <-ch:
			if !synced {
				t.Errorf("the %s watcher is not synced at the instant it starts delivering events", name)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s watcher never synced", name)
		}
	}
}
