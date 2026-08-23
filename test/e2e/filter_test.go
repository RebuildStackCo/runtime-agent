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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

func TestPodFilterAgainstRealCluster(t *testing.T) {
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	base := fmt.Sprintf("runtime-agent-e2e-flt-%d", os.Getpid())
	nsCollected := base
	nsDenied := base + "-denied"
	nsOptedOut := base + "-opted"

	makeNamespace := func(name string, annotations map[string]string) {
		t.Helper()
		_, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("creating namespace %s: %v", name, err)
		}
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
			defer cleanupCancel()
			_ = clientset.CoreV1().Namespaces().Delete(cleanupCtx, name, metav1.DeleteOptions{})
		})
	}
	makeNamespace(nsCollected, nil)
	makeNamespace(nsDenied, nil)
	makeNamespace(nsOptedOut, map[string]string{collector.CollectAnnotation: "false"})

	makePod := func(namespace, name string, annotations map[string]string) {
		t.Helper()
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Annotations: annotations},
			Spec: corev1.PodSpec{
				Containers:    []corev1.Container{{Name: "main", Image: pauseImage}},
				RestartPolicy: corev1.RestartPolicyNever,
			},
		}
		if _, err := clientset.CoreV1().Pods(namespace).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating pod %s/%s: %v", namespace, name, err)
		}
	}
	makePod(nsCollected, "visible", nil)
	makePod(nsCollected, "self-opted", map[string]string{collector.CollectAnnotation: "false"})
	makePod(nsDenied, "hidden-by-config", nil)
	makePod(nsOptedOut, "hidden-by-annotation", nil)

	var mu sync.Mutex
	seen := make(map[string]collector.PodInfo)
	watcher := collector.NewPodWatcher(clientset, func(p collector.PodInfo) {
		observed, _ := json.Marshal(p)
		t.Logf("pod observed: %s", observed)
		if p.Namespace != nsCollected && p.Namespace != nsDenied && p.Namespace != nsOptedOut {
			return
		}
		mu.Lock()
		seen[p.Namespace+"/"+p.Name] = p
		mu.Unlock()
	})
	filter := collector.NewFilter(nil, []string{nsDenied})
	watcher.SetFilter(filter)
	watcherDone := make(chan error, 1)
	go func() { watcherDone <- watcher.Run(ctx) }()

	// The three excluded pods are visible only through the counters —
	// exactly as designed: identities of excluded objects stay aggregate.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		c := filter.Snapshot()
		if c.ExcludedNamespaceFilter >= 1 && c.ExcludedNamespaceAnnotation >= 1 && c.ExcludedPodAnnotation >= 1 {
			coverage, _ := json.Marshal(c)
			t.Logf("coverage: %s", coverage)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("coverage = %+v before timeout, want every exclusion type counted", c)
		}
		time.Sleep(500 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if _, ok := seen[nsCollected+"/visible"]; !ok {
		t.Errorf("pod %s/visible was not reported; saw %v", nsCollected, seen)
	}
	for _, excluded := range []string{
		nsCollected + "/self-opted",
		nsDenied + "/hidden-by-config",
		nsOptedOut + "/hidden-by-annotation",
	} {
		if _, ok := seen[excluded]; ok {
			t.Errorf("excluded pod %s was reported", excluded)
		}
	}

	cancel()
	if err := <-watcherDone; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
}
