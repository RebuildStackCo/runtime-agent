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

// TestNodeScannerAgainstRealCluster deploys the node-role DaemonSet from the
// shipped manifest into kind alongside a known Go workload, then reads the
// DaemonSet's structured log and asserts the scanner found the workload with
// the right module path and Go version, and filtered infrastructure (the agent
// filters at least itself). It exercises the real image and the real manifest,
// including the zero-RBAC ServiceAccount and the hardened securityContext.
//
// It is gated on E2E_AGENT_IMAGE / E2E_SAMPLE_IMAGE (kind-loaded images); use
// `make node-e2e`. Without them the test skips, since the in-process e2e run
// (`make e2e`) has no images to deploy.
func TestNodeScannerAgainstRealCluster(t *testing.T) {
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
	// namespace and the kind-loaded image. No controller endpoint here — this
	// test asserts on the node's own log, log-only mode.
	deployNodeDaemonSet(ctx, t, clientset, ns, agentImage, "")

	nodePod := waitDaemonSetPod(ctx, t, clientset, ns)
	t.Logf("node pod: %s", nodePod)

	// Poll the node role's log until it reports the sample workload found and
	// at least one infrastructure binary filtered (the agent filters itself).
	deadline := time.Now().Add(5 * time.Minute)
	for {
		detected, coverage := scanLog(ctx, t, clientset, ns, nodePod)
		if detected != nil && coverage != nil {
			if detected.GoVersion == "" {
				t.Errorf("sample detected but go_version empty: %+v", detected)
			}
			if !strings.HasPrefix(detected.GoVersion, "go1.") {
				t.Errorf("sample go_version = %q, want a go1.x version", detected.GoVersion)
			}
			if coverage.FilteredInfra < 1 {
				t.Errorf("filtered_infra = %d, want >= 1 (the agent filters at least itself)", coverage.FilteredInfra)
			}
			if coverage.ProcessesScanned < 1 {
				t.Errorf("processes_scanned = %d, want >= 1", coverage.ProcessesScanned)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node role did not report the sample workload (%s) and a filtered-infra coverage line before timeout", sampleModulePath)
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
	GoFound          int    `json:"go_found"`
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
func deployNodeDaemonSet(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, image, controllerEndpoint string) {
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
