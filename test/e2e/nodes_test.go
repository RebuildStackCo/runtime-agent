//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

func TestNodeWatcherAgainstRealCluster(t *testing.T) {
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var mu sync.Mutex
	seen := make(map[string]collector.NodeInfo)
	watcher := collector.NewNodeWatcher(clientset, func(n collector.NodeInfo) {
		observed, _ := json.Marshal(n)
		t.Logf("node observed: %s", observed)
		mu.Lock()
		seen[n.Name] = n
		mu.Unlock()
	})
	watcherDone := make(chan error, 1)
	go func() { watcherDone <- watcher.Run(ctx) }()

	// A kind cluster always has at least the control-plane node; wait until
	// the watcher reports something.
	deadline := time.Now().Add(time.Minute)
	for {
		mu.Lock()
		count := len(seen)
		mu.Unlock()
		if count > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no nodes reported before timeout")
		}
		time.Sleep(500 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for name, n := range seen {
		if n.AllocatableCPUMilli <= 0 || n.AllocatableMemoryBytes <= 0 {
			t.Errorf("node %s: allocatable cpu/memory = %d/%d, want > 0",
				name, n.AllocatableCPUMilli, n.AllocatableMemoryBytes)
		}
		if n.CapacityCPUMilli < n.AllocatableCPUMilli || n.CapacityMemoryBytes < n.AllocatableMemoryBytes {
			t.Errorf("node %s: capacity (%d cpu / %d mem) below allocatable (%d cpu / %d mem)",
				name, n.CapacityCPUMilli, n.CapacityMemoryBytes, n.AllocatableCPUMilli, n.AllocatableMemoryBytes)
		}
	}

	cancel()
	if err := <-watcherDone; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
}
