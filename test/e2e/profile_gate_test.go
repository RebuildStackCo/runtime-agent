//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TestEBPFGateRefusesGracefully is the gate-refusal e2e that runs anywhere,
// including kind on linuxkit where the kernel has no BTF. It asserts the
// graceful path of ADR 0011 §2: the pod still starts, the profiler refuses with
// a distinct reason, and the Go-binary scanner keeps running. The full capture
// path needs a Linux+BTF host and is a separate target.
//
// Gated on E2E_AGENT_IMAGE (kind-loaded); use `make profile-gate-e2e`.
func TestEBPFGateRefusesGracefully(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	if agentImage == "" {
		t.Skip("E2E_AGENT_IMAGE not set; run `make profile-gate-e2e`")
	}
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-gate-e2e-%d", os.Getpid())
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		_ = clientset.CoreV1().Namespaces().Delete(cleanupCtx, ns, metav1.DeleteOptions{})
	})

	deployNodeEBPFLogOnly(ctx, t, clientset, ns, agentImage)
	pod := waitDaemonSetPod(ctx, t, clientset, ns)

	// This test asserts the graceful *refusal* path (a node that grants the eBPF
	// caps but whose kernel lacks BTF or is too old). It skips the two cases it
	// cannot assert: a node that cannot grant CAP_BPF/CAP_PERFMON at all (the pod
	// never starts, so the in-binary gate never runs — e.g. Docker Desktop /
	// linuxkit), and a node that fully supports eBPF (the gate does not refuse;
	// that path is the capture e2e).
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if reason, failed := capApplyFailed(ctx, clientset, ns, pod); failed {
			t.Skipf("node cannot grant CAP_BPF/CAP_PERFMON, so the eBPF profile pod never starts and the "+
				"in-binary gate never runs (e.g. Docker Desktop / linuxkit): %s", reason)
		}
		refused, coverage, ready := ebpfGateLog(ctx, t, clientset, ns, pod)
		if ready {
			t.Skip("node supports eBPF and the gate did not refuse; the full capture path is covered by profile-capture-e2e")
		}
		if refused != nil && coverage {
			switch refused.Reason {
			case "btf_absent", "kernel_too_old":
				return // graceful refusal AND the scanner kept running
			default:
				t.Fatalf("unexpected gate refusal reason %q (want btf_absent or kernel_too_old)", refused.Reason)
			}
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("did not observe a gate refusal, eBPF-ready, or a cap-apply failure before timeout (pod %s/%s)", ns, pod)
}

// capApplyFailed reports whether the pod's container failed to start because the
// node could not apply CAP_BPF/CAP_PERFMON — the container init fails at runc
// before the binary runs, so no log is produced.
func capApplyFailed(ctx context.Context, cs kubernetes.Interface, ns, pod string) (string, bool) {
	p, err := cs.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil || len(p.Status.ContainerStatuses) == 0 {
		return "", false
	}
	msg := ""
	if term := p.Status.ContainerStatuses[0].LastTerminationState.Terminated; term != nil {
		msg = term.Message
	}
	if strings.Contains(msg, "apply caps") || strings.Contains(msg, "capabilities") {
		return strings.TrimSpace(msg), true
	}
	return "", false
}

// gateLine is the subset of the node's structured log this test reads.
type gateLine struct {
	Msg    string `json:"msg"`
	Reason string `json:"reason"`
}

// ebpfGateLog streams the node pod's log and reports the eBPF-refusal line (with
// its reason), whether the scanner emitted a coverage line, and whether the gate
// reported the profiler ready.
func ebpfGateLog(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, pod string) (refused *gateLine, coverage, ready bool) {
	t.Helper()
	stream, err := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		t.Logf("getting logs for %s (will retry): %v", pod, err)
		return nil, false, false
	}
	defer func() { _ = stream.Close() }()

	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var l gateLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(l.Msg, "ebpf profile refused"):
			cp := l
			refused = &cp
		case l.Msg == "ebpf profile ready":
			ready = true
		case l.Msg == "scan coverage":
			coverage = true
		}
	}
	return refused, coverage, ready
}

// deployNodeEBPFLogOnly installs the node half of the `ebpf` profile with no
// controller and no endpoints.
//
// The gate refuses before any query or ship would happen on a node without BTF,
// so wiring the endpoints would add nothing to observe. What is under test is
// the chart's own eBPF wiring: the two capabilities, the /sys/kernel mount and
// the profiling config it renders.
func deployNodeEBPFLogOnly(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, image string) {
	t.Helper()
	installChart(ctx, t, cs, ns, image, installOptions{
		profile:        "ebpf",
		skipController: true,
		values: map[string]any{
			"node": map[string]any{"scanInterval": "15s"},
			"profiling": map[string]any{
				"allowedModulePrefixes": []any{sampleModulePath},
			},
		},
		mutateNode: func(ds *appsv1.DaemonSet) {
			setNodeArgs(ds, "node", "-proc", "/host/proc", "-interval", "15s",
				"-enable-ebpf", "-config", "/etc/runtime-agent/config.yaml", "-sys", "/sys")
		},
	})
}
