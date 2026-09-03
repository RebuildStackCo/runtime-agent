//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

func TestOOMKillDetectionAgainstRealCluster(t *testing.T) {
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-e2e-oom-%d", os.Getpid())
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = clientset.CoreV1().Namespaces().Delete(cleanupCtx, ns, metav1.DeleteOptions{})
	})

	// The shell doubles a string until the 32Mi memory limit kills it.
	// The shell is PID 1, so the OOM kill terminates the whole container
	// and the runtime marks it OOMKilled. restartPolicy Never keeps the
	// kill in state.terminated, deterministic to assert on.
	memoryHog := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "memory-hog"},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "hog",
				Image:   "docker.io/library/busybox:1.36",
				Command: []string{"sh", "-c", `s=x; while true; do s="$s$s"; done`},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
				},
			}},
		},
	}
	if _, err := clientset.CoreV1().Pods(ns).Create(ctx, memoryHog, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating pod: %v", err)
	}

	var mu sync.Mutex
	var kills []model.OOMKill
	watcher := collector.NewPodWatcher(clientset, func(model.PodInfo) {})
	watcher.OnOOMKill(func(o model.OOMKill) {
		observed, _ := json.Marshal(o)
		t.Logf("oom kill observed: %s", observed)
		if o.Namespace != ns {
			return
		}
		mu.Lock()
		kills = append(kills, o)
		mu.Unlock()
	})
	watcherDone := make(chan error, 1)
	go func() { watcherDone <- watcher.Run(ctx) }()

	// Image pull plus a few seconds of allocation; 3 minutes is generous.
	deadline := time.Now().Add(3 * time.Minute)
	var got model.OOMKill
	for {
		mu.Lock()
		if len(kills) > 0 {
			got = kills[0]
		}
		mu.Unlock()
		if got.Pod != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no OOM kill reported before timeout")
		}
		time.Sleep(time.Second)
	}

	if got.Pod != "memory-hog" || got.Container != "hog" {
		t.Errorf("kill = %s/%s, want memory-hog/hog", got.Pod, got.Container)
	}
	if got.ExitCode != 137 {
		t.Errorf("exit code = %d, want 137", got.ExitCode)
	}
	if got.MemoryLimitBytes == nil || *got.MemoryLimitBytes != 32<<20 {
		t.Errorf("memory limit = %v, want %d", got.MemoryLimitBytes, int64(32<<20))
	}
	if got.Workload.Kind != "none" {
		t.Errorf("workload kind = %q, want none (bare pod)", got.Workload.Kind)
	}

	cancel()
	if err := <-watcherDone; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
}
