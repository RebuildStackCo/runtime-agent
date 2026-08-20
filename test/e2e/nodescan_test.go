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

// The module path baked into test/e2e/sample: neither infrastructure nor this
// agent, so the scanner keeps it. Must match test/e2e/sample/go.mod.
const sampleModulePath = "example.com/rebuildstack-e2e/goworkload"

const nodeManifestPath = "../../deploy/node-daemonset.yaml"

// TestNodeScannerFailsClosedWithoutScope deploys the node-role DaemonSet from
// the shipped manifest into kind alongside a known Go workload, with no scope
// endpoint, and asserts the scanner reports nothing about it.
//
// This is the security property of ADR 0015 proven against the real image and
// the real manifest: a node that cannot ask the controller which pods passed the
// customer's filters must scan none of them, because its cgroup gives it a pod
// UID and never a namespace. A Go workload really is running on the node here —
// the same one the inventory e2e finds — and it must still not appear, in the
// payload or in the log.
//
// The positive path (workload found, joined, spooled) is TestGoInventoryEndToEnd,
// which deploys a controller and asserts on the payload rather than the log.
//
// It is gated on E2E_AGENT_IMAGE / E2E_SAMPLE_IMAGE (kind-loaded images); use
// `make node-e2e`. Without them the test skips, since the in-process e2e run
// (`make e2e`) has no images to deploy.
func TestNodeScannerFailsClosedWithoutScope(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	sampleImage := os.Getenv("E2E_SAMPLE_IMAGE")
	if agentImage == "" || sampleImage == "" {
		t.Skip("E2E_AGENT_IMAGE / E2E_SAMPLE_IMAGE not set; run `make node-e2e`")
	}
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-node-e2e-%d", os.Getpid())
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

	// The known Go process. Deploy it first and wait until it is Running, so it
	// is present in the process table before the scanner's first pass.
	sample := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "goworkload"},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:            "goworkload",
				Image:           sampleImage,
				ImagePullPolicy: corev1.PullNever, // kind-loaded, not in a registry
			}},
		},
	}
	if _, err := clientset.CoreV1().Pods(ns).Create(ctx, sample, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating sample workload: %v", err)
	}
	waitPodRunning(ctx, t, clientset, ns, "goworkload")

	// Deploy the node role from the shipped manifest, retargeted at this
	// namespace and the kind-loaded image, with neither a controller nor a scope
	// endpoint — the misconfiguration this test is about.
	deployNodeDaemonSet(ctx, t, clientset, ns, agentImage, "", "")

	nodePod := waitDaemonSetPod(ctx, t, clientset, ns)
	t.Logf("node pod: %s", nodePod)

	// Poll until the scanner has completed a pass. It must have walked the
	// process table (so the sample workload was reachable) and kept none of it.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		detected, coverage := scanLog(ctx, t, clientset, ns, nodePod)
		if detected != nil {
			t.Fatalf("the scanner reported a workload with no scope: %+v", detected)
		}
		if coverage != nil && coverage.ProcessesScanned > 0 {
			if coverage.GoFound != 0 {
				t.Errorf("go_found = %d, want 0 — nothing may be kept without a scope", coverage.GoFound)
			}
			if coverage.PodsInScope != 0 {
				t.Errorf("pods_in_scope = %d, want 0", coverage.PodsInScope)
			}
			if coverage.FilteredScope < 1 {
				t.Errorf("filtered_scope = %d, want >= 1 (every process dropped on its cgroup)",
					coverage.FilteredScope)
			}
			// Executables are never opened for out-of-scope processes, so the
			// unreadable counter cannot move either: nothing is collected, as
			// opposed to collected and dropped.
			if coverage.Unreadable != 0 {
				t.Errorf("unreadable = %d, want 0 — no executable may be read out of scope", coverage.Unreadable)
			}
			if coverage.FilteredInfra != 0 {
				t.Errorf("filtered_infra = %d, want 0 — module paths are never extracted out of scope",
					coverage.FilteredInfra)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("node role never reported a scan coverage line before timeout")
		}
		time.Sleep(3 * time.Second)
	}
}

// logLine is the subset of the node role's JSON log we assert on.
type logLine struct {
	Msg              string `json:"msg"`
	MainModule       string `json:"main_module"`
	GoVersion        string `json:"go_version"`
	ProcessesScanned int    `json:"processes_scanned"`
	PodsInScope      int    `json:"pods_in_scope"`
	GoFound          int    `json:"go_found"`
	FilteredScope    int    `json:"filtered_scope"`
	FilteredInfra    int    `json:"filtered_infra"`
	Unreadable       int    `json:"unreadable"`
}

// scanLog streams the node pod's log and returns the detection line for the
// sample workload and the latest coverage line, if present.
func scanLog(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, pod string) (detected, coverage *logLine) {
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
		var l logLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue // non-JSON line (e.g. a runtime panic banner); ignore
		}
		switch l.Msg {
		case "go binary detected":
			if l.MainModule == sampleModulePath {
				cp := l
				detected = &cp
			}
		case "scan coverage":
			cp := l
			coverage = &cp
		}
	}
	return detected, coverage
}

// deployNodeDaemonSet decodes the shipped manifest and applies its
// ServiceAccount and DaemonSet into ns, overriding only the namespace, the
// image, the pull policy, and the scan interval. When controllerEndpoint is
// non-empty, the -controller-endpoint arg is added so the node ships its
// findings there (ADR 0010); empty runs the node log-only. The zero-RBAC
// ServiceAccount (no Role/RoleBinding is created anywhere), the projected
// controller-audience token volume, and the securityContext are applied exactly
// as written.
func deployNodeDaemonSet(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, image, controllerEndpoint, scopeEndpoint string) {
	t.Helper()
	data, err := os.ReadFile(nodeManifestPath)
	if err != nil {
		t.Fatalf("reading manifest %s: %v", nodeManifestPath, err)
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
		if tm.Kind == "" {
			continue // a comment-only section between --- separators
		}
		switch tm.Kind {
		case "ServiceAccount":
			var sa corev1.ServiceAccount
			if err := k8syaml.Unmarshal(doc, &sa); err != nil {
				t.Fatalf("decoding ServiceAccount: %v", err)
			}
			sa.Namespace = ns
			if _, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx, &sa, metav1.CreateOptions{}); err != nil {
				t.Fatalf("creating ServiceAccount: %v", err)
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
			// Shorten the interval so the test observes repeated passes quickly;
			// the first pass runs at startup regardless.
			c.Args = []string{"node", "-proc", "/host/proc", "-interval", "15s"}
			if controllerEndpoint != "" {
				c.Args = append(c.Args, "-controller-endpoint", controllerEndpoint)
			}
			// Without a scope endpoint the node fails closed and scans nothing
			// (ADR 0015); passing "" is how a test exercises that path.
			if scopeEndpoint != "" {
				c.Args = append(c.Args, "-scope-endpoint", scopeEndpoint)
			}
			if _, err := cs.AppsV1().DaemonSets(ns).Create(ctx, &ds, metav1.CreateOptions{}); err != nil {
				t.Fatalf("creating DaemonSet: %v", err)
			}
		case "Namespace":
			// The test manages its own namespace; ignore the manifest's.
		default:
			t.Fatalf("unexpected manifest kind %q", tm.Kind)
		}
	}
}

func waitPodRunning(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && p.Status.Phase == corev1.PodRunning {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("pod %s/%s did not reach Running before timeout", ns, name)
}

// waitDaemonSetPod waits for the node DaemonSet's single pod (kind has one node)
// to reach Running and returns its name.
func waitDaemonSetPod(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns string) string {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/component=node",
		})
		if err == nil {
			for _, p := range pods.Items {
				if p.Status.Phase == corev1.PodRunning {
					return p.Name
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("node DaemonSet pod did not reach Running before timeout")
	return ""
}
