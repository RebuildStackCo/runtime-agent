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

	"github.com/google/pprof/profile"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	k8syaml "sigs.k8s.io/yaml"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeprofile"
)

// thirdPartyMarker is a substring of the third-party dependency's package path
// (github.com/cespare/xxhash) that the sample calls from its hot loop. After the
// node's symbol filter runs, no frame carrying it may leave the node.
const thirdPartyMarker = "xxhash"

// TestEBPFCaptureEndToEnd is the full-path eBPF capture e2e (ADR 0011): a Go
// workload burns CPU in an allow-listed function that also calls a third-party
// dependency; the node loads the eBPF profiler, asks the controller which
// containers to profile, captures and symbolizes stacks, filters symbols on the
// node, and ships the filtered pprof; the controller joins it to the workload and
// spools an ebpf_profile payload. The test asserts on that payload — the bytes
// the backend would receive: a valid cpu/nanoseconds profile whose own-module
// frames survive and whose third-party frames are redacted to [filtered].
//
// The capture path needs a Linux host with kernel BTF and CAP_BPF/CAP_PERFMON.
// Where the node cannot grant those caps or the kernel lacks BTF (Docker Desktop
// / linuxkit), the pod never captures and the test skips with a clear reason —
// the same detection the gate-refusal e2e uses. Run `make profile-capture-e2e`
// on a capable host (a real Linux VM).
//
// Gated on E2E_AGENT_IMAGE / E2E_SAMPLE_IMAGE (kind-loaded).
func TestEBPFCaptureEndToEnd(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	sampleImage := os.Getenv("E2E_SAMPLE_IMAGE")
	if agentImage == "" || sampleImage == "" {
		t.Skip("E2E_AGENT_IMAGE / E2E_SAMPLE_IMAGE not set; run `make profile-capture-e2e`")
	}
	config := clusterConfig(t)
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-cap-e2e-%d", os.Getpid())
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

	// The CPU-hot Go workload as a Deployment, so the controller resolves it to a
	// workload (Pod → ReplicaSet → Deployment) and the profiler has a busy target.
	deploySampleWorkload(ctx, t, clientset, ns, sampleImage)

	// The controller with profiling enabled and only this sample eligible, so the
	// targets reply names exactly the sample's containers on the querying node.
	deployController(ctx, t, clientset, ns, agentImage, renderControllerConfigProfiling(ns))
	controllerPod := waitDeploymentPod(ctx, t, clientset, ns, "controller")
	t.Logf("controller pod: %s", controllerPod)

	// The eBPF node DaemonSet, pointed at the controller Service and carrying the
	// node-side profiling config (symbol allow-list, short capture cadence).
	deployNodeDaemonSetEBPFCapture(ctx, t, clientset, ns, agentImage)
	nodePod := waitDaemonSetPod(ctx, t, clientset, ns)
	t.Logf("node pod: %s", nodePod)

	// Wait for the eBPF gate to report ready. If the node cannot grant the caps or
	// the kernel lacks BTF, skip — the capture path is physically unavailable here.
	waitForEBPFReadyOrSkip(ctx, t, clientset, ns, nodePod)

	// Poll the controller's spool for the sample's ebpf_profile payload, then
	// assert on the pprof bytes.
	deadline := time.Now().Add(8 * time.Minute)
	for {
		if pprof, ok := readSampleProfile(ctx, t, config, clientset, ns, controllerPod); ok {
			assertProfile(t, pprof)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("controller spool never carried an ebpf_profile for the sample workload before timeout")
		}
		time.Sleep(5 * time.Second)
	}
}

// waitForEBPFReadyOrSkip blocks until the node's eBPF profiler actually starts
// capturing (the only signal that the full path can be asserted), or skips the
// test when capture is unavailable in this environment. The gate reporting
// "ready" is NOT sufficient: the kernel-version + BTF gate can pass and the
// tracer still fail to load — most notably inside kind's nested container, where
// the profiler's system-analysis step fails with program_load_failed even though
// the gate passed (docs/spikes/ebpf-capture-e2e.md). Skips cover every case where
// capture cannot run here: caps not grantable, the gate refusing (btf_absent /
// kernel_too_old), or the tracer failing to load (program_load_failed).
func waitForEBPFReadyOrSkip(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, pod string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if reason, failed := capApplyFailed(ctx, cs, ns, pod); failed {
			t.Skipf("node cannot grant CAP_BPF/CAP_PERFMON, so the eBPF profiler never starts "+
				"(e.g. Docker Desktop / linuxkit): %s", reason)
		}
		st := readEBPFNodeState(ctx, t, cs, ns, pod)
		if st.captureStarted {
			return // the tracer loaded and is capturing; assert the full path
		}
		if st.captureUnavailable {
			t.Skipf("eBPF gate passed but the profiler could not load here (reason %q); the capture "+
				"path needs a host where the eBPF tracer loads — not kind's nested container, whose "+
				"system-analysis step fails (see docs/spikes/ebpf-capture-e2e.md)", st.unavailableReason)
		}
		switch st.refusedReason {
		case "btf_absent", "kernel_too_old":
			t.Skipf("node kernel cannot support eBPF capture (reason %q); the capture path needs "+
				"a Linux host with BTF", st.refusedReason)
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("eBPF profiler neither started capturing nor reported unavailable before timeout (pod %s/%s)", ns, pod)
}

// ebpfNodeState is the capture-relevant subset of the node's structured log.
type ebpfNodeState struct {
	captureStarted     bool   // the eBPF tracer loaded and is capturing
	captureUnavailable bool   // the tracer failed to load; node degraded to scanner
	unavailableReason  string // reason on captureUnavailable (e.g. program_load_failed)
	refusedReason      string // reason on a gate refusal (btf_absent / kernel_too_old)
}

// readEBPFNodeState streams the node pod's log and reports what the eBPF profiler
// did: gate ready, tracer started, or tracer unavailable / gate refused (with the
// reason). It reads the whole log each call, so the latest state wins.
func readEBPFNodeState(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, pod string) ebpfNodeState {
	t.Helper()
	var st ebpfNodeState
	stream, err := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		t.Logf("getting logs for %s (will retry): %v", pod, err)
		return st
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
		case l.Msg == "ebpf capture started":
			st.captureStarted = true
		case strings.HasPrefix(l.Msg, "ebpf capture unavailable"):
			st.captureUnavailable = true
			st.unavailableReason = l.Reason
		case strings.HasPrefix(l.Msg, "ebpf profile refused"):
			st.refusedReason = l.Reason
		}
	}
	return st
}

// spoolProfilePayload mirrors the fields of an ebpf_profile spool file this test
// asserts on (internal/sink profilePayload).
type spoolProfilePayload struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Container string `json:"container"`
	Pprof     []byte `json:"pprof"`
}

// readSampleProfile lists the controller spool for the sample's profile file and,
// if present, returns its parsed pprof. The file name is
// profile-<ns>-<workload>-<container>-<digest>-<start>-<end>.json (internal/sink);
// the digest is what keeps two replicas' captures of one window apart (ADR 0023),
// so the prefix match below is deliberately blind to everything after the
// container name.
func readSampleProfile(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod string) (*profile.Profile, bool) {
	t.Helper()
	listing, ok := execSpoolReader(ctx, t, config, cs, ns, pod, []string{"ls", "/var/spool/runtime-agent"})
	if !ok {
		return nil, false
	}
	name := ""
	for _, line := range strings.Fields(listing) {
		if strings.HasPrefix(line, "profile-"+ns+"-goworkload-") && strings.HasSuffix(line, ".json") {
			name = line
			break
		}
	}
	if name == "" {
		return nil, false
	}
	raw, ok := execSpoolReader(ctx, t, config, cs, ns, pod, []string{"cat", "/var/spool/runtime-agent/" + name})
	if !ok {
		return nil, false
	}
	var payload spoolProfilePayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Logf("profile file %s not valid JSON yet (will retry): %v", name, err)
		return nil, false
	}
	if payload.Kind != "ebpf_profile" {
		t.Errorf("payload kind = %q, want ebpf_profile", payload.Kind)
	}
	if payload.Workload != "goworkload" {
		t.Errorf("workload = %q, want goworkload", payload.Workload)
	}
	p, err := profile.ParseData(payload.Pprof)
	if err != nil {
		t.Fatalf("parsing shipped pprof from %s: %v", name, err)
	}
	return p, true
}

// assertProfile checks the shipped profile is the one the backend would want and
// that the node symbol filter did its job (ADR 0011 §4–5): a cpu/nanoseconds
// profile in which the workload's own allow-listed function survived, no
// third-party frame leaked, and redaction actually happened.
func assertProfile(t *testing.T, p *profile.Profile) {
	t.Helper()

	hasCPUNanos := false
	for _, st := range p.SampleType {
		if st.Type == "cpu" && st.Unit == "nanoseconds" {
			hasCPUNanos = true
		}
	}
	if !hasCPUNanos {
		t.Errorf("shipped profile has no cpu/nanoseconds sample type; sample types = %v", p.SampleType)
	}

	names := make(map[string]struct{}, len(p.Function))
	for _, fn := range p.Function {
		names[fn.Name] = struct{}{}
	}

	keptOwn := false
	leaked := ""
	redacted := false
	for name := range names {
		switch {
		case strings.HasPrefix(name, sampleModulePath):
			keptOwn = true // the allow-listed own-module frame survived
		case strings.Contains(name, thirdPartyMarker):
			leaked = name // a third-party frame that must have been redacted
		case name == nodeprofile.RedactedFrame:
			redacted = true // redaction placeholder is present
		}
	}

	if !keptOwn {
		t.Errorf("no allow-listed own-module frame (prefix %q) in the shipped profile; "+
			"function names = %v", sampleModulePath, keys(names))
	}
	if leaked != "" {
		t.Errorf("third-party frame %q leaked past the node filter (want redacted to %q)",
			leaked, nodeprofile.RedactedFrame)
	}
	if !redacted {
		t.Errorf("no %q placeholder in the shipped profile; the third-party frames the sample "+
			"generates should have been redacted", nodeprofile.RedactedFrame)
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// execSpoolReader runs cmd in the controller pod's spool-reader sidecar and
// returns its stdout. A non-zero exit (file not written yet) returns ok=false so
// the caller retries.
func execSpoolReader(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod string, cmd []string) (string, bool) {
	t.Helper()
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "spool-reader",
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		t.Fatalf("building exec: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return "", false
	}
	return stdout.String(), true
}

// renderControllerConfigProfiling is renderControllerConfig plus an enabled
// profiling block scoped to this namespace's sample: the controller opens the
// targets endpoint and ranks only the goworkload Deployment, so the reply names
// exactly the sample's containers.
func renderControllerConfigProfiling(ns string) string {
	return fmt.Sprintf(`spool:
  dir: /var/spool/runtime-agent
nodeIntake:
  enabled: true
  listenAddress: ":8080"
  audience: rebuildstack-controller
  expectedSubject: "system:serviceaccount:%s:runtime-agent-node"
profiling:
  enabled: true
  eligibleNamespaces:
    - %s
  eligibleWorkloads:
    - goworkload
  topN: 5
`, ns, ns)
}

// renderNodeProfilingConfig is the node-side profiling config: the symbol
// allow-list is exactly the sample's own module (so its frames survive and every
// third-party frame is redacted), third-party symbols drop, and a short capture
// cadence so the test sees a profile quickly. A higher overhead ceiling raises the
// sampling rate, which shortens time-to-signal in the test.
//
// It carries no eligible set. That is the controller's (see
// renderControllerConfigProfiling), and since ADR 0025 the node's schema rejects
// it outright — a node given a setting it cannot enforce fails to start rather
// than parsing and ignoring it.
func renderNodeProfilingConfig() string {
	return fmt.Sprintf(`profiling:
  allowedModulePrefixes:
    - %s
  thirdPartySymbols: drop
  maxTargetsPerWindow: 5
  captureDurationSeconds: 30
  intervalSeconds: 60
  overheadCeilingPercent: 20
`, sampleModulePath)
}

// deployNodeDaemonSetEBPFCapture applies the ebpf node variant into ns for the
// capture path: it installs the node-side profiling config, points the node at
// this namespace's controller Service (inventory, targets, and profile
// endpoints), and shortens the scan interval. Unlike the gate-refusal deploy
// (log-only), this one wires the endpoints so a ready node actually queries and
// ships.
func deployNodeDaemonSetEBPFCapture(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, image string) {
	t.Helper()
	data, err := os.ReadFile(nodeEBPFManifestPath)
	if err != nil {
		t.Fatalf("reading manifest %s: %v", nodeEBPFManifestPath, err)
	}
	base := fmt.Sprintf("http://runtime-agent-controller.%s.svc.cluster.local:8080", ns)
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
		case "", "Namespace":
			// The test manages its own namespace.
		case "ServiceAccount":
			var sa corev1.ServiceAccount
			mustUnmarshal(t, doc, &sa)
			sa.Namespace = ns
			create(ctx, t, "ServiceAccount", func() error {
				_, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx, &sa, metav1.CreateOptions{})
				return err
			})
		case "ConfigMap":
			var cm corev1.ConfigMap
			mustUnmarshal(t, doc, &cm)
			cm.Namespace = ns
			cm.Data["config.yaml"] = renderNodeProfilingConfig()
			create(ctx, t, "ConfigMap", func() error {
				_, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &cm, metav1.CreateOptions{})
				return err
			})
		case "DaemonSet":
			var ds appsv1.DaemonSet
			mustUnmarshal(t, doc, &ds)
			ds.Namespace = ns
			c := &ds.Spec.Template.Spec.Containers[0]
			c.Image = image
			c.ImagePullPolicy = corev1.PullNever
			c.Args = []string{
				"node", "-proc", "/host/proc", "-interval", "30s",
				"-enable-ebpf", "-config", "/etc/runtime-agent/config.yaml", "-sys", "/sys",
				"-controller-endpoint", base + "/v1/node-inventory",
				"-targets-endpoint", base + "/v1/node-targets",
				"-profile-endpoint", base + "/v1/node-profile",
			}
			create(ctx, t, "DaemonSet", func() error {
				_, err := cs.AppsV1().DaemonSets(ns).Create(ctx, &ds, metav1.CreateOptions{})
				return err
			})
		default:
			t.Fatalf("unexpected manifest kind %q", tm.Kind)
		}
	}
}
