//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	k8syaml "sigs.k8s.io/yaml"
)

const nodeEBPFManifestPath = "../../deploy/node-daemonset-ebpf.yaml"

// TestEBPFGateRefusesGracefully is the gate-refusal e2e that runs anywhere,
// including kind on Docker Desktop / linuxkit where the kernel has no BTF. It
// deploys the `ebpf` node variant and asserts the graceful path (ADR 0011 §2):
// the pod still starts (so the manifest does not hard-require BTF), the eBPF
// profiler refuses with a distinct reason (btf_absent or kernel_too_old), and
// the Go-binary scanner keeps running. The full capture path needs a Linux+BTF
// host and is a separate target.
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

	deployNodeDaemonSetEBPF(ctx, t, clientset, ns, agentImage)

	// Reaching Running is itself an assertion: the ebpf manifest must not hard-
	// require BTF, or the pod would never start on a no-BTF node.
	pod := waitDaemonSetPod(ctx, t, clientset, ns)

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		refused, coverage := ebpfGateLog(ctx, t, clientset, ns, pod)
		if refused != nil && coverage != nil {
			switch refused.Reason {
			case "btf_absent", "kernel_too_old":
				// graceful refusal on a node that cannot support eBPF
			default:
				t.Fatalf("unexpected gate refusal reason %q (want btf_absent or kernel_too_old)", refused.Reason)
			}
			return // gate refused AND the scanner produced a coverage line
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("did not observe both an eBPF gate refusal and a scanner coverage line before timeout (pod %s/%s)", ns, pod)
}

// gateLine is the subset of the node's structured log this test reads.
type gateLine struct {
	Msg    string `json:"msg"`
	Reason string `json:"reason"`
}

// ebpfGateLog streams the node pod's log and returns the eBPF-refusal line (with
// its reason) and whether the scanner emitted a coverage line, if present.
func ebpfGateLog(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, pod string) (refused, coverage *gateLine) {
	t.Helper()
	stream, err := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		t.Logf("getting logs for %s (will retry): %v", pod, err)
		return nil, nil
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
		case l.Msg == "scan coverage":
			cp := l
			coverage = &cp
		}
	}
	return refused, coverage
}

// deployNodeDaemonSetEBPF applies the `ebpf` variant manifest into ns: its
// ServiceAccount, its profiling ConfigMap, and its DaemonSet, overriding only the
// namespace, the image, the pull policy, and the args. It runs the profiler in
// log-only mode (no endpoints) — on a no-BTF node the gate refuses before any
// query or ship would happen anyway.
func deployNodeDaemonSetEBPF(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, image string) {
	t.Helper()
	data, err := os.ReadFile(nodeEBPFManifestPath)
	if err != nil {
		t.Fatalf("reading manifest %s: %v", nodeEBPFManifestPath, err)
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading manifest doc: %v", err)
		}
		if strings.TrimSpace(string(doc)) == "" {
			continue
		}
		var tm metav1.TypeMeta
		if err := k8syaml.Unmarshal(doc, &tm); err != nil {
			t.Fatalf("decoding doc TypeMeta: %v", err)
		}
		switch tm.Kind {
		case "":
			continue // comment-only section
		case "Namespace":
			// The test manages its own namespace.
		case "ServiceAccount":
			var sa corev1.ServiceAccount
			if err := k8syaml.Unmarshal(doc, &sa); err != nil {
				t.Fatalf("decoding ServiceAccount: %v", err)
			}
			sa.Namespace = ns
			if _, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx, &sa, metav1.CreateOptions{}); err != nil {
				t.Fatalf("creating ServiceAccount: %v", err)
			}
		case "ConfigMap":
			var cm corev1.ConfigMap
			if err := k8syaml.Unmarshal(doc, &cm); err != nil {
				t.Fatalf("decoding ConfigMap: %v", err)
			}
			cm.Namespace = ns
			if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &cm, metav1.CreateOptions{}); err != nil {
				t.Fatalf("creating ConfigMap: %v", err)
			}
		case "DaemonSet":
			var ds appsv1.DaemonSet
			if err := k8syaml.Unmarshal(doc, &ds); err != nil {
				t.Fatalf("decoding DaemonSet: %v", err)
			}
			ds.Namespace = ns
			c := &ds.Spec.Template.Spec.Containers[0]
			c.Image = image
			c.ImagePullPolicy = corev1.PullNever
			// Log-only: keep the eBPF gate and the config, drop the controller
			// endpoints; shorten the interval so the scanner logs a pass quickly.
			c.Args = []string{
				"node", "-proc", "/host/proc", "-interval", "15s",
				"-enable-ebpf", "-config", "/etc/runtime-agent/config.yaml", "-sys", "/sys",
			}
			if _, err := cs.AppsV1().DaemonSets(ns).Create(ctx, &ds, metav1.CreateOptions{}); err != nil {
				t.Fatalf("creating DaemonSet: %v", err)
			}
		default:
			t.Fatalf("unexpected manifest kind %q", tm.Kind)
		}
	}
}
