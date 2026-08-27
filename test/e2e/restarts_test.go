//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	restartsSpoolGlob = spoolDir + "/restarts-*.json"
	countersSpoolPath = spoolDir + "/restart-counters.json"
)

// TestContainerRestartsEndToEnd runs a container that exits immediately and
// asserts the controller's restart journal in a real cluster (ADR 0020): the
// crash loop is counted, attributed, and its reason and exit code carried.
//
// The controller is deployed before the workload on purpose: a container the
// agent has never seen is only baselined, so a pod already looping at startup
// produces nothing until its next restart. Gated on E2E_AGENT_IMAGE.
func TestContainerRestartsEndToEnd(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	if agentImage == "" {
		t.Skip("E2E_AGENT_IMAGE not set; run `make restarts-e2e`")
	}
	config := clusterConfig(t)
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-restart-e2e-%d", os.Getpid())
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

	deployController(ctx, t, clientset, ns, agentImage)
	controllerPod := waitDeploymentPod(ctx, t, clientset, ns, "controller")
	t.Logf("controller pod: %s", controllerPod)

	deployCrashLoop(ctx, t, clientset, ns)

	// The kubelet backs off between restarts (10s, 20s, 40s …) and the
	// controller flushes once a minute, so allow several rounds.
	deadline := time.Now().Add(6 * time.Minute)
	for {
		if rec, ok := findCrashLoopRecord(ctx, t, config, clientset, ns, controllerPod); ok {
			t.Logf("restart record: %+v", rec)
			if rec.Restarts < 1 {
				t.Errorf("restarts = %d, want at least 1", rec.Restarts)
			}
			if rec.Container != "crashloop" {
				t.Errorf("container = %q, want crashloop", rec.Container)
			}
			if rec.Workload.Kind != "Deployment" || rec.Workload.Name != "crashloop" {
				t.Errorf("workload = %s/%s, want Deployment/crashloop", rec.Workload.Kind, rec.Workload.Name)
			}
			// The container exits 1, which the runtime reports as "Error".
			if rec.Reasons["Error"] < 1 {
				t.Errorf("reasons = %v, want at least one Error", rec.Reasons)
			}
			if rec.LastExitCode == nil || *rec.LastExitCode != 1 {
				t.Errorf("last_exit_code = %v, want 1", rec.LastExitCode)
			}
			// The arithmetic the payload promises: what was seen plus what was
			// not equals the counter's own total.
			var observed int64
			for _, n := range rec.Reasons {
				observed += n
			}
			if observed+rec.ReasonsUnobserved != rec.Restarts {
				t.Errorf("reasons %v plus %d unobserved != %d restarts",
					rec.Reasons, rec.ReasonsUnobserved, rec.Restarts)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the container_restarts payload never carried the crash-looping workload before timeout")
		}
		time.Sleep(5 * time.Second)
	}
}

// TestRestartCountersCarryTheHistoryTheAgentDidNotWatch is the install-day
// scenario: the crash loop runs first, the agent arrives into a cluster already
// failing. The exact inverse of the test above, where the controller is deployed
// first so the journal can baseline before the first restart. The journal stays
// correctly silent about what preceded it, and `restart_counters` is what
// carries those restarts out anyway (ADR 0034).
//
// Gated on E2E_AGENT_IMAGE (kind-loaded); use `make restarts-e2e`.
func TestRestartCountersCarryTheHistoryTheAgentDidNotWatch(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	if agentImage == "" {
		t.Skip("E2E_AGENT_IMAGE not set; run `make restarts-e2e`")
	}
	config := clusterConfig(t)
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-counter-e2e-%d", os.Getpid())
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

	// The history the agent will never see happen.
	deployCrashLoop(ctx, t, clientset, ns)
	priorRestarts := waitForRestartCount(ctx, t, clientset, ns, 2)
	t.Logf("the workload restarted %d times before the agent existed", priorRestarts)

	deployController(ctx, t, clientset, ns, agentImage)
	controllerPod := waitDeploymentPod(ctx, t, clientset, ns, "controller")
	t.Logf("controller pod: %s", controllerPod)

	deadline := time.Now().Add(5 * time.Minute)
	for {
		if rec, ok := findCounterRecord(ctx, t, config, clientset, ns, controllerPod); ok {
			t.Logf("counter reading: %+v", rec)
			// The claim the whole payload exists to make: restarts that
			// happened before this agent process, reported by it.
			if rec.RestartsBeforeObservation < priorRestarts {
				t.Errorf("restarts_before_observation = %d, want at least the %d that happened before the agent started",
					rec.RestartsBeforeObservation, priorRestarts)
			}
			if rec.Restarts < rec.RestartsBeforeObservation {
				t.Errorf("restarts = %d is below restarts_before_observation = %d; the total cannot be smaller than its own past",
					rec.Restarts, rec.RestartsBeforeObservation)
			}
			// The interval those restarts are spread over must be a real one:
			// the pod existed before the agent looked at it.
			if !rec.PodCreatedAt.Before(rec.ObservedSince) {
				t.Errorf("pod_created_at %s is not before observed_since %s; the interval is degenerate",
					rec.PodCreatedAt, rec.ObservedSince)
			}
			if rec.Workload.Kind != "Deployment" || rec.Workload.Name != "crashloop" {
				t.Errorf("workload = %s/%s, want Deployment/crashloop", rec.Workload.Kind, rec.Workload.Name)
			}
			if rec.LastTermination == nil || rec.LastTermination.ExitCode != 1 {
				t.Errorf("last_termination = %+v, want exit code 1", rec.LastTermination)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the restart_counters payload never carried the crash-looping container before timeout")
		}
		time.Sleep(5 * time.Second)
	}
}

// waitForRestartCount blocks until the crash-looping container's counter, read
// from the API rather than from the agent, reaches want — and returns the value
// it actually reached.
//
// Reading it from Kubernetes is what makes the assertion mean something: the
// number the payload must carry is established independently of the agent that
// carries it.
func waitForRestartCount(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns string, want int32) int64 {
	t.Helper()
	// The kubelet backs off 10s, 20s, 40s … between restarts, so two of them
	// take under a minute but not much under.
	deadline := time.Now().Add(4 * time.Minute)
	for {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=crashloop"})
		if err != nil {
			t.Fatalf("listing crash-loop pods: %v", err)
		}
		for _, pod := range pods.Items {
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == "crashloop" && status.RestartCount >= want {
					return int64(status.RestartCount)
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the crash-loop container never reached %d restarts before timeout", want)
		}
		time.Sleep(5 * time.Second)
	}
}

// counterRecord mirrors one record of the restart_counters payload.
type counterRecord struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Workload  struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"workload"`
	Restarts                  int64      `json:"restarts"`
	RestartsBeforeObservation int64      `json:"restarts_before_observation"`
	ObservedSince             time.Time  `json:"observed_since"`
	PodCreatedAt              time.Time  `json:"pod_created_at"`
	ContainerStartedAt        *time.Time `json:"container_started_at"`
	LastTermination           *struct {
		Reason     string    `json:"reason"`
		ExitCode   int32     `json:"exit_code"`
		FinishedAt time.Time `json:"finished_at"`
	} `json:"last_termination"`
}

// findCounterRecord reads the counter snapshot from the controller's spool and
// returns the crash-looping container's record, if it is there yet.
func findCounterRecord(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod string) (counterRecord, bool) {
	t.Helper()
	raw, ok := readSpoolGlob(ctx, t, config, cs, ns, pod, countersSpoolPath)
	if !ok {
		return counterRecord{}, false
	}
	var payload struct {
		Kind       string          `json:"kind"`
		Source     string          `json:"source"`
		CapturedAt time.Time       `json:"captured_at"`
		Records    []counterRecord `json:"records"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Logf("counter payload not valid JSON yet (will retry): %v", err)
		return counterRecord{}, false
	}
	if payload.Kind != "restart_counters" {
		t.Errorf("payload kind = %q, want restart_counters", payload.Kind)
	}
	if payload.Source != "journal" {
		t.Errorf("payload source = %q, want journal", payload.Source)
	}
	if payload.CapturedAt.IsZero() {
		t.Error("captured_at is zero; a reading without its instant is not a reading")
	}
	for _, r := range payload.Records {
		if r.Namespace == ns && r.Container == "crashloop" {
			return r, true
		}
	}
	return counterRecord{}, false
}

// restartRecord mirrors one record of the container_restarts payload.
type restartRecord struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Workload  struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"workload"`
	Restarts          int64            `json:"restarts"`
	Reasons           map[string]int64 `json:"reasons"`
	ReasonsUnobserved int64            `json:"reasons_unobserved"`
	LastExitCode      *int32           `json:"last_exit_code"`
}

// findCrashLoopRecord reads the newest restart window from the controller's
// spool and returns the record for the crash-looping container, if it is there
// yet.
func findCrashLoopRecord(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod string) (restartRecord, bool) {
	t.Helper()
	raw, ok := readSpoolGlob(ctx, t, config, cs, ns, pod, restartsSpoolGlob)
	if !ok {
		return restartRecord{}, false
	}
	var payload struct {
		Kind    string          `json:"kind"`
		Source  string          `json:"source"`
		Records []restartRecord `json:"records"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Logf("restart payload not valid JSON yet (will retry): %v", err)
		return restartRecord{}, false
	}
	if payload.Kind != "container_restarts" {
		t.Errorf("payload kind = %q, want container_restarts", payload.Kind)
	}
	if payload.Source != "journal" {
		t.Errorf("payload source = %q, want journal", payload.Source)
	}
	for _, r := range payload.Records {
		if r.Namespace == ns && r.Container == "crashloop" {
			return r, true
		}
	}
	return restartRecord{}, false
}

// readSpoolGlob cats whichever spool file matches the pattern. The restart
// payload's filename carries its window start, which the test cannot know, so
// unlike readSpoolFile this one goes through the sidecar's shell.
func readSpoolGlob(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod, pattern string) (string, bool) {
	t.Helper()
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "spool-reader",
			Command:   []string{"sh", "-c", "cat " + pattern},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		t.Fatalf("building exec: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		// No file matches yet — expected during the poll, not a failure.
		return "", false
	}
	return stdout.String(), true
}

// deployCrashLoop creates a Deployment whose container exits non-zero at once,
// so the kubelet restarts it on its backoff schedule. It reuses the spool
// reader's image, which is already loaded into the cluster.
func deployCrashLoop(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns string) {
	t.Helper()
	replicas := int32(1)
	labels := map[string]string{"app": "crashloop"}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "crashloop"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "crashloop",
						Image:           spoolReaderImage(),
						ImagePullPolicy: corev1.PullNever,
						// A short life, then exit 1: long enough for the kubelet
						// to report the container running at least once, so the
						// agent takes its baseline before the first restart.
						Command: []string{"sh", "-c", "sleep 2; exit 1"},
					}},
				},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating crash-loop deployment: %v", err)
	}
	t.Logf("crash-loop workload created in %s", ns)
}
