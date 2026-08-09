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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

const busyboxImage = "docker.io/library/busybox:1.36"

// TestUsagePollerAgainstRealCluster runs the full usage pipeline — pod
// watcher (attribution), node watcher (poll targets), kubelet poller —
// against kind, with a deployment that burns CPU, and asserts that snapshot
// records accumulate real core-nanoseconds and memory samples for it.
func TestUsagePollerAgainstRealCluster(t *testing.T) {
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-e2e-usage-%d", os.Getpid())
	_, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = clientset.CoreV1().Namespaces().Delete(cleanupCtx, ns, metav1.DeleteOptions{})
	})

	// A tight shell loop burns CPU up to the 300m limit — enough to
	// accumulate unambiguous core-nanoseconds within two poll intervals.
	burner := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "burner"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "burner"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "burner"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "burn",
						Image:   busyboxImage,
						Command: []string{"sh", "-c", "while :; do :; done"},
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("300m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
					}},
				},
			},
		},
	}
	if _, err := clientset.AppsV1().Deployments(ns).Create(ctx, burner, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating burner deployment: %v", err)
	}

	records := startUsagePipeline(ctx, t, clientset)

	// A snapshot record for the burner must accumulate at least 3 CPU
	// core-seconds (300m for ~10 s of attributed runtime) and real memory
	// samples. Snapshots arrive once a minute; give the image pull, the
	// poll cadence, and two snapshot ticks room.
	deadline := time.Now().Add(5 * time.Minute)
	var last *rollup.Record
	for time.Now().Before(deadline) {
		if r := records.get(ns, "burner"); r != nil {
			last = r
			if r.CPU.CoreNanoseconds > 3e9 && r.CPU.Samples > 0 && r.Memory.Samples > 0 {
				assertBurnerRecord(t, r)
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no burner snapshot with enough usage before deadline; last record: %+v", last)
}

func assertBurnerRecord(t *testing.T, r *rollup.Record) {
	t.Helper()
	encoded, _ := json.Marshal(r)
	t.Logf("burner record: %s", encoded)

	if r.WindowSeconds != 3600 {
		t.Errorf("window seconds = %d, want 3600", r.WindowSeconds)
	}
	if !r.WindowStart.Equal(r.WindowStart.Truncate(time.Hour)) {
		t.Errorf("window start %v is not hour-aligned", r.WindowStart)
	}
	if r.Container != "burn" {
		t.Errorf("container = %q, want burn", r.Container)
	}
	// The limit caps the sampled rate at ~300 millicores; anything wildly
	// above means the delta math is wrong. CFS enforcement wobbles, so
	// allow slack.
	if r.CPU.MaxMilli > 450 {
		t.Errorf("max sampled rate = %d milli, want ≤ ~450 under a 300m limit", r.CPU.MaxMilli)
	}
	if r.Memory.MaxBytes <= 0 || r.Memory.MaxBytes > 64<<20 {
		t.Errorf("memory max = %d bytes, want within the 64Mi limit", r.Memory.MaxBytes)
	}
	if got, want := int64(r.CPU.Hist.Count()), r.CPU.Samples; got != want { // #nosec G115 -- sample counts are tiny
		t.Errorf("cpu histogram count = %d, want %d (one per sample)", got, want)
	}
}

// recordSink collects the latest snapshot record per workload.
type recordSink struct {
	mu     sync.Mutex
	latest map[string]*rollup.Record
}

func (s *recordSink) put(records []*rollup.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range records {
		s.latest[r.Namespace+"/"+r.WorkloadName] = r
	}
}

func (s *recordSink) get(namespace, workload string) *rollup.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest[namespace+"/"+workload]
}

// startUsagePipeline wires pod watcher, node watcher, and usage poller the
// same way the agent's main does, and returns the sink receiving snapshots.
func startUsagePipeline(ctx context.Context, t *testing.T, clientset kubernetes.Interface) *recordSink {
	t.Helper()
	sink := &recordSink{latest: make(map[string]*rollup.Record)}

	podWatcher := collector.NewPodWatcher(clientset, func(collector.PodInfo) {})
	nodeWatcher := collector.NewNodeWatcher(clientset, func(collector.NodeInfo) {})
	poller := collector.NewUsagePoller(clientset, nodeWatcher.Names, podWatcher,
		func(sequence int64, records []*rollup.Record) {
			t.Logf("usage snapshot %d: %d records", sequence, len(records))
			sink.put(records)
		},
		func(records []*rollup.Record) { sink.put(records) },
		func(node string, err error) { t.Logf("kubelet poll failed on %s: %v", node, err) },
	)

	for name, run := range map[string]func(context.Context) error{
		"pods": podWatcher.Run, "nodes": nodeWatcher.Run, "usage": poller.Run,
	} {
		go func() {
			if err := run(ctx); err != nil && ctx.Err() == nil {
				t.Errorf("%s runner failed: %v", name, err)
			}
		}()
	}
	return sink
}
