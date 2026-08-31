//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeprofile"
)

// thirdPartyMarker is a substring of the third-party dependency's package path
// (github.com/cespare/xxhash) that the sample calls from its hot loop. After the
// node's symbol filter runs, no frame carrying it may leave the node.
const thirdPartyMarker = "xxhash"

// TestEBPFCaptureEndToEnd is the full-path capture e2e (ADR 0011): a Go workload
// burns CPU in an allow-listed function that calls a third-party dependency, and
// the test asserts on the shipped payload — a valid cpu/nanoseconds profile whose
// own-module frames survive and whose third-party frames read [filtered].
//
// Needs a Linux host with kernel BTF and CAP_BPF/CAP_PERFMON; elsewhere the pod
// never captures and the test skips with a reason. Gated on E2E_AGENT_IMAGE and
// E2E_SAMPLE_IMAGE; run `make profile-capture-e2e` on a capable host.
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

	// The `ebpf` profile: the controller with profiling enabled and only this
	// sample eligible, and the node carrying the symbol allow-list and the short
	// capture cadence. One install, because that is how a customer gets them.
	installChart(ctx, t, clientset, ns, agentImage, installOptions{
		profile:     "ebpf",
		spoolReader: true,
		values:      captureValues(ns),
	})
	controllerPod := waitDeploymentPod(ctx, t, clientset, ns, "controller")
	t.Logf("controller pod: %s", controllerPod)
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

// waitForEBPFReadyOrSkip blocks until the profiler actually starts capturing —
// the only signal that the full path can be asserted — or skips when capture is
// unavailable here. The gate reporting "ready" is not sufficient: it can pass
// and the tracer still fail to load, notably inside kind's nested container
// (docs/spikes/ebpf-capture-e2e.md). Skips cover caps not grantable, the gate
// refusing, and the tracer failing to load.
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

// captureValues is the whole of what this test configures, and every line is a
// value a customer could set. Scope comes from the collection filter and
// nothing else (ADR 0025).
//
// **The symbol allow-list is deliberately absent.** It used to name the
// sample's own module, configuring the test around the defect ADR 0059 closes;
// with nothing set, a surviving sample frame can only mean the node read the
// module off the binary.
func captureValues(ns string) map[string]any {
	return map[string]any{
		"filters": map[string]any{
			"namespaces": map[string]any{"allow": []any{ns}},
		},
		"profiling": map[string]any{
			"topN":                   5,
			"thirdPartySymbols":      "drop",
			"captureDurationSeconds": 30,
			"intervalSeconds":        60,
			"overheadCeilingPercent": 20,
		},
		"node": map[string]any{"scanInterval": "30s"},
	}
}
