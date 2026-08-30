//go:build e2e

package e2e

import (
	"bytes"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	k8syaml "sigs.k8s.io/yaml"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

// spoolReaderImage is a shell-bearing image loaded into kind as a sidecar in
// the controller pod: the agent image is distroless (no shell), so the sidecar
// is how the test reads the spool file the controller writes. `make
// inventory-e2e` loads it.
func spoolReaderImage() string {
	if img := os.Getenv("E2E_SPOOL_READER_IMAGE"); img != "" {
		return img
	}
	return "busybox:1.37"
}

const spoolDir = "/var/spool/runtime-agent"
const spoolPath = spoolDir + "/go-inventory.json"

const nodeMetadataSpoolPath = spoolDir + "/node-metadata.json"

const processPeaksSpoolPath = spoolDir + "/process-peaks.json"

const listeningPortsSpoolPath = spoolDir + "/listening-ports.json"

const coverageSpoolPath = spoolDir + "/collection-coverage.json"

// buildSpoolPath mirrors the sink's filename rule for a build payload: one file
// per image digest, with the digest's colon replaced so the name is filesystem-
// and tooling-safe (internal/sink.digestFileToken).
func buildSpoolPath(digest string) string {
	return spoolDir + "/go-build-" + strings.ReplaceAll(digest, ":", "-") + ".json"
}

// allowedBuildSettings restates the scanner's allow-list here on purpose. The
// unit test checks the filter; this list checks what a real cluster actually
// shipped, and restating it means widening the list in the scanner alone cannot
// silently widen what leaves the cluster (ADR 0019).
var allowedBuildSettings = map[string]struct{}{
	"CGO_ENABLED":  {},
	"GOARCH":       {},
	"GOAMD64":      {},
	"GOARM64":      {},
	"GOARM":        {},
	"-race":        {},
	"-trimpath":    {},
	"vcs":          {},
	"vcs.revision": {},
	"vcs.time":     {},
	"vcs.modified": {},
}

// TestGoInventoryEndToEnd exercises the whole node→controller inventory path in
// a real cluster (ADR 0010): a Go workload runs, the node scans it and ships the
// fact with a projected token, the controller validates it against the cluster
// JWKS, joins it, and spools a go_inventory payload. The assertion is on the
// spool contents, not on logs.
//
// Gated on E2E_AGENT_IMAGE / E2E_SAMPLE_IMAGE; use `make inventory-e2e`.
func TestGoInventoryEndToEnd(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	sampleImage := os.Getenv("E2E_SAMPLE_IMAGE")
	if agentImage == "" || sampleImage == "" {
		t.Skip("E2E_AGENT_IMAGE / E2E_SAMPLE_IMAGE not set; run `make inventory-e2e`")
	}
	config := clusterConfig(t)
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-inv-e2e-%d", os.Getpid())
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

	// The known Go workload as a Deployment, so the controller resolves it to a
	// workload (Pod → ReplicaSet → Deployment) — exercising the owner chain.
	deploySampleWorkload(ctx, t, clientset, ns, sampleImage)

	// The `inventory` profile: the controller with its read RBAC, JWKS discovery
	// grant and node-intake receiver, and the node DaemonSet running the scanner.
	// One install, because that is how a customer gets them — and the endpoints
	// the node calls are the chart's own, derived from this namespace rather than
	// assembled by the test.
	installChart(ctx, t, clientset, ns, agentImage, installOptions{
		profile:     "inventory",
		spoolReader: true,
		values:      map[string]any{"node": map[string]any{"scanInterval": "15s"}},
	})
	controllerPod := waitDeploymentPod(ctx, t, clientset, ns, "controller")
	t.Logf("controller pod: %s", controllerPod)
	nodePod := waitDaemonSetPod(ctx, t, clientset, ns)
	t.Logf("node pod: %s", nodePod)

	// Poll the controller's spool until the go_inventory payload carries the
	// sample workload. Delivery is periodic (the node's scan interval and the
	// controller's flush cadence), so allow generous time.
	deadline := time.Now().Add(6 * time.Minute)
	for {
		if rec, cov, ok := findSampleRecord(ctx, t, config, clientset, ns, controllerPod); ok {
			// The payload says how complete it is: the node that scanned this
			// workload reported, and its facts joined. Without this block an
			// empty inventory and a fleet that never checked in look identical.
			if cov.NodesReported < 1 {
				t.Errorf("coverage.nodes_reported = %d, want >= 1", cov.NodesReported)
			}
			if cov.FactsJoined < 1 {
				t.Errorf("coverage.facts_joined = %d, want >= 1", cov.FactsJoined)
			}
			if cov.Since.IsZero() {
				t.Error("coverage.since is zero; the base of the counters must travel with them")
			}
			if !strings.HasPrefix(rec.GoVersion, "go1.") {
				t.Errorf("go_version = %q, want a go1.x version", rec.GoVersion)
			}
			if rec.ImageDigest == "" {
				t.Errorf("image_digest empty; want the sample image content digest")
			}
			if !strings.HasPrefix(rec.ImageDigest, "sha256:") {
				t.Errorf("image_digest = %q, want a sha256: digest", rec.ImageDigest)
			}
			if rec.PGO {
				t.Errorf("pgo = true; the sample is built without PGO")
			}
			if rec.Container != "goworkload" {
				t.Errorf("container = %q, want goworkload", rec.Container)
			}
			if rec.WorkloadKind != "Deployment" || rec.WorkloadName != "goworkload" {
				t.Errorf("workload = %s/%s, want Deployment/goworkload", rec.WorkloadKind, rec.WorkloadName)
			}
			checkSampleBuild(ctx, t, config, clientset, ns, controllerPod, rec.ImageDigest)
			checkSamplePeak(ctx, t, config, clientset, ns, controllerPod, rec.ImageDigest)
			checkSamplePorts(ctx, t, config, clientset, ns, controllerPod, rec.ImageDigest)
			checkEndpointConfirmed(ctx, t, config, clientset, ns, controllerPod)
			checkNodeArchitecture(ctx, t, config, clientset, ns, controllerPod)
			checkCoverageReported(ctx, t, config, clientset, ns, controllerPod)
			checkOptOutRemovesTheRecord(ctx, t, config, clientset, ns, controllerPod)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("go_inventory payload never carried the sample workload (%s) before timeout", sampleModulePath)
		}
		time.Sleep(5 * time.Second)
	}
}

// checkOptOutRemovesTheRecord annotates the running sample pod with the opt-out
// annotation and asserts its record leaves the go_inventory payload.
//
// The promise of docs/security.md §11 — "the exclusion applies at the collection
// stage" — proven against the payload rather than the filter. Opting a pod out
// only stops new facts arriving, so without the retention pass of ADR 0018 the
// record already held would keep shipping for as long as the controller ran.
func checkOptOutRemovesTheRecord(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, controllerPod string) {
	t.Helper()
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=goworkload"})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("listing the sample pod: %v (%d found)", err, len(pods.Items))
	}
	name := pods.Items[0].Name
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:"false"}}}`, collector.CollectAnnotation)
	if _, err := cs.CoreV1().Pods(ns).Patch(ctx, name, types.StrategicMergePatchType,
		[]byte(patch), metav1.PatchOptions{}); err != nil {
		t.Fatalf("annotating %s/%s to opt out: %v", ns, name, err)
	}
	t.Logf("opted %s/%s out; waiting for its record to leave the payload", ns, name)

	// Eviction happens on the controller's flush, so allow several cadences.
	deadline := time.Now().Add(4 * time.Minute)
	for {
		if _, _, ok := findSampleRecord(ctx, t, config, cs, ns, controllerPod); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the opted-out workload (%s) was still in the go_inventory payload after 4 minutes", sampleModulePath)
		}
		time.Sleep(5 * time.Second)
	}
}

// goInventoryRecord mirrors one record of the go_inventory payload.
type goInventoryRecord struct {
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workload_kind"`
	WorkloadName string `json:"workload_name"`
	Container    string `json:"container"`
	GoVersion    string `json:"go_version"`
	ModulePath   string `json:"module_path"`
	ImageDigest  string `json:"image_digest"`
	PGO          bool   `json:"pgo"`
}

// inventoryCoverage mirrors the completeness block of the go_inventory payload.
type inventoryCoverage struct {
	Since         time.Time `json:"since"`
	NodesReported int       `json:"nodes_reported"`
	FactsReceived int64     `json:"facts_received"`
	FactsJoined   int64     `json:"facts_joined"`
}

// findSampleRecord reads the controller's spool file via the sidecar and returns
// the record for the sample module and the payload's coverage block, if the
// payload exists yet.
func findSampleRecord(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod string) (goInventoryRecord, inventoryCoverage, bool) {
	t.Helper()
	raw, ok := readSpoolFile(ctx, t, config, cs, ns, pod, spoolPath)
	if !ok {
		return goInventoryRecord{}, inventoryCoverage{}, false
	}
	var payload struct {
		Kind     string              `json:"kind"`
		Coverage inventoryCoverage   `json:"coverage"`
		Records  []goInventoryRecord `json:"records"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Logf("spool file not valid JSON yet (will retry): %v", err)
		return goInventoryRecord{}, inventoryCoverage{}, false
	}
	if payload.Kind != "go_inventory" {
		t.Errorf("payload kind = %q, want go_inventory", payload.Kind)
	}
	for _, r := range payload.Records {
		if r.ModulePath == sampleModulePath {
			return r, payload.Coverage, true
		}
	}
	return goInventoryRecord{}, inventoryCoverage{}, false
}

// checkCoverageReported asserts that the agent says what it did, from a real
// cluster: what it observed, which of its reads worked, and the shape of the
// configuration — without a name of anything excluded (ADR 0054).
func checkCoverageReported(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod string) {
	t.Helper()
	raw, ok := readSpoolFile(ctx, t, config, cs, ns, pod, coverageSpoolPath)
	if !ok {
		t.Errorf("collection_coverage payload never appeared at %s", coverageSpoolPath)
		return
	}
	var payload struct {
		Kind   string `json:"kind"`
		Source string `json:"source"`
		Agent  struct {
			Version string `json:"version"`
			Config  struct {
				Since             string `json:"since"`
				NamespacesAllowed int    `json:"namespaces_allowed"`
				NamespacesDenied  int    `json:"namespaces_denied"`
			} `json:"config"`
			UsageSignals []string `json:"usage_signals"`
		} `json:"agent"`
		Sources []struct {
			Name    string `json:"name"`
			Synced  bool   `json:"synced"`
			Failing bool   `json:"failing"`
		} `json:"sources"`
		Filter struct {
			PodsObserved int64 `json:"pods_observed"`
		} `json:"filter"`
		Scan struct {
			Nodes            int `json:"nodes"`
			ProcessesScanned int `json:"processes_scanned"`
			GoFound          int `json:"go_found"`
			FilteredScope    int `json:"filtered_scope"`
		} `json:"scan"`
		Pprof struct {
			Confirmed   int `json:"confirmed"`
			Absent      int `json:"absent"`
			Unreachable int `json:"unreachable"`
		} `json:"pprof"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("coverage payload not valid JSON: %v", err)
	}
	if payload.Kind != "collection_coverage" || payload.Source != "agent" {
		t.Errorf("kind/source = %q/%q", payload.Kind, payload.Source)
	}
	if payload.Filter.PodsObserved == 0 {
		t.Error("pods_observed = 0; the agent claims to have looked at nothing")
	}
	if payload.Agent.Config.Since == "" {
		t.Error("config.since is empty; nothing dates the configuration in force")
	}
	// Measured access, not declared: every source the chart granted should have
	// filled its cache on a healthy cluster.
	if len(payload.Sources) == 0 {
		t.Error("sources is empty; the payload says nothing about what the agent could read")
	}
	for _, s := range payload.Sources {
		if !s.Synced || s.Failing {
			t.Errorf("source %s: synced=%v failing=%v, want a healthy cache", s.Name, s.Synced, s.Failing)
		}
	}
	// The node half arrived over the channel and was not dropped at the join.
	if payload.Scan.Nodes == 0 || payload.Scan.ProcessesScanned == 0 || payload.Scan.GoFound == 0 {
		t.Errorf("scan = %+v; the node scanners' own counters did not reach the payload", payload.Scan)
	}
	t.Logf("coverage: %d pods observed, %d processes scanned on %d node(s) (%d Go, %d out of scope), signals %v",
		payload.Filter.PodsObserved, payload.Scan.ProcessesScanned, payload.Scan.Nodes,
		payload.Scan.GoFound, payload.Scan.FilteredScope, payload.Agent.UsageSignals)
	t.Logf("pprof endpoints: %d confirmed, %d absent, %d unreachable",
		payload.Pprof.Confirmed, payload.Pprof.Absent, payload.Pprof.Unreachable)
}

// checkEndpointConfirmed asserts that the controller really opened a connection
// to the sample and recognized its pprof index. The sample serves it on
// 0.0.0.0:6060 and declares that port nowhere, so a confirmation proves the
// whole funnel end to end: linked package, bound port, one request (ADR 0057).
//
// Discovery runs on its own tick after the node's facts arrive, so this polls
// rather than reading once.
func checkEndpointConfirmed(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for {
		raw, ok := readSpoolFile(ctx, t, config, cs, ns, pod, coverageSpoolPath)
		if ok {
			var payload struct {
				Pprof struct {
					Confirmed   int `json:"confirmed"`
					Absent      int `json:"absent"`
					Unreachable int `json:"unreachable"`
				} `json:"pprof"`
			}
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				t.Fatalf("coverage payload not valid JSON: %v", err)
			}
			if payload.Pprof.Confirmed > 0 {
				t.Logf("endpoint discovery confirmed %d target(s)", payload.Pprof.Confirmed)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Errorf("no pprof endpoint was ever confirmed; the sample serves one on :6060")
			return
		}
		time.Sleep(10 * time.Second)
	}
}

// checkSamplePeak asserts that the measured half of the same node reports
// arrived as its own payload: a real high-water mark, read from
// `/proc/<pid>/status` on the node, for the sample's own build (ADR 0052).
//
// It is written on the same flush as the inventory snapshot but after it, so a
// read can land between the two writes: poll briefly rather than fail on the
// race, exactly as the build payload does.
func checkSamplePeak(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod, digest string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		raw, ok := readSpoolFile(ctx, t, config, cs, ns, pod, processPeaksSpoolPath)
		if ok {
			var payload struct {
				Kind    string `json:"kind"`
				Source  string `json:"source"`
				Records []struct {
					WorkloadName   string `json:"workload_name"`
					Container      string `json:"container"`
					ImageDigest    string `json:"image_digest"`
					PeakRSSBytes   int64  `json:"peak_rss_bytes"`
					Processes      int    `json:"processes"`
					CPUsAllowedMin int    `json:"cpus_allowed_min"`
					CPUsAllowedMax int    `json:"cpus_allowed_max"`
				} `json:"records"`
			}
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				t.Fatalf("process peaks payload not valid JSON: %v", err)
			}
			for _, r := range payload.Records {
				if r.WorkloadName != "goworkload" {
					continue
				}
				if payload.Kind != "process_peaks" || payload.Source != "measured" {
					t.Errorf("kind/source = %q/%q, want process_peaks/measured", payload.Kind, payload.Source)
				}
				// The kernel's own number for a process that is running: it
				// cannot be zero, and no Go runtime starts in under 256 KiB.
				// The bound stays well under what the sample actually reaches
				// (~1.7 MB) so a leaner sample does not turn this into a flake.
				if r.PeakRSSBytes < 1<<18 {
					t.Errorf("peak_rss_bytes = %d, want a real high-water mark", r.PeakRSSBytes)
				}
				if r.Processes < 1 {
					t.Errorf("processes = %d, want at least the one replica", r.Processes)
				}
				if r.CPUsAllowedMin < 1 || r.CPUsAllowedMax < r.CPUsAllowedMin {
					t.Errorf("cpus allowed = %d..%d, want a sane range", r.CPUsAllowedMin, r.CPUsAllowedMax)
				}
				// The peak belongs to the build that reached it, so it joins to
				// the inventory record's digest and to `go_build`.
				if r.ImageDigest != digest {
					t.Errorf("image_digest = %q, want %q — the peak must key to the build", r.ImageDigest, digest)
				}
				t.Logf("peak for %s/%s: %d bytes over %d process(es), %d..%d CPUs allowed",
					r.WorkloadName, r.Container, r.PeakRSSBytes, r.Processes, r.CPUsAllowedMin, r.CPUsAllowedMax)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Errorf("process_peaks payload never carried the sample workload")
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// checkSamplePorts asserts that the ports the sample really binds arrived, with
// the reachability of each. The sample listens on 0.0.0.0:6060 and on
// 127.0.0.1:9090 and declares neither in its spec, so this also proves the read
// is of the process and not of `containerPorts` (ADR 0056 §2).
func checkSamplePorts(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod, digest string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		raw, ok := readSpoolFile(ctx, t, config, cs, ns, pod, listeningPortsSpoolPath)
		if ok {
			var payload struct {
				Kind    string `json:"kind"`
				Source  string `json:"source"`
				Records []struct {
					WorkloadName string `json:"workload_name"`
					Container    string `json:"container"`
					ImageDigest  string `json:"image_digest"`
					Ports        []struct {
						Port     int  `json:"port"`
						Loopback bool `json:"loopback"`
					} `json:"ports"`
				} `json:"records"`
			}
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				t.Fatalf("listening ports payload not valid JSON: %v", err)
			}
			for _, r := range payload.Records {
				if r.WorkloadName != "goworkload" {
					continue
				}
				if payload.Kind != "listening_ports" || payload.Source != "structural" {
					t.Errorf("kind/source = %q/%q, want listening_ports/structural", payload.Kind, payload.Source)
				}
				if r.ImageDigest != digest {
					t.Errorf("image_digest = %q, want %q — a port keys to the build that binds it", r.ImageDigest, digest)
				}
				want := map[int]bool{6060: false, 9090: true}
				got := map[int]bool{}
				for _, port := range r.Ports {
					got[port.Port] = port.Loopback
				}
				for port, loopback := range want {
					have, ok := got[port]
					if !ok {
						t.Errorf("port %d absent; the sample binds it", port)
						continue
					}
					if have != loopback {
						t.Errorf("port %d loopback = %v, want %v", port, have, loopback)
					}
				}
				t.Logf("ports for %s/%s: %+v", r.WorkloadName, r.Container, r.Ports)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Errorf("listening_ports payload never carried the sample workload")
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// checkSampleBuild asserts that the build payload for the sample reached the
// spool and carries the module the sample really imports and the settings the
// sample was really built with. It is written on the same flush as the inventory
// snapshot but after it, so a read can land between the two writes: poll briefly
// rather than fail on the race.
func checkSampleBuild(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod, digest string) {
	t.Helper()
	if digest == "" {
		return // already reported as an empty image_digest
	}
	path := buildSpoolPath(digest)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		raw, ok := readSpoolFile(ctx, t, config, cs, ns, pod, path)
		if ok {
			var payload struct {
				Kind        string `json:"kind"`
				ImageDigest string `json:"image_digest"`
				MainModule  string `json:"main_module"`
				Modules     []struct {
					Path     string `json:"path"`
					Version  string `json:"version"`
					Replaced bool   `json:"replaced"`
				} `json:"modules"`
				Settings      map[string]string `json:"settings"`
				PprofEndpoint bool              `json:"pprof_endpoint"`
			}
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				t.Fatalf("build payload not valid JSON: %v", err)
			}
			if payload.Kind != "go_build" {
				t.Errorf("payload kind = %q, want go_build", payload.Kind)
			}
			if payload.ImageDigest != digest {
				t.Errorf("image_digest = %q, want %q — the payload must key to the build it describes",
					payload.ImageDigest, digest)
			}
			if payload.MainModule != sampleModulePath {
				t.Errorf("main_module = %q, want %q", payload.MainModule, sampleModulePath)
			}
			// The sample really imports this module (test/e2e/sample/go.mod), so
			// its absence means the dependency set was lost in the join — the
			// defect this payload exists to close. It carries the version the
			// toolchain recorded (ADR 0048 §3).
			found := false
			for _, m := range payload.Modules {
				if m.Path == sampleDependency {
					found = true
					if m.Version == "" {
						t.Errorf("module %q carries no version", m.Path)
					}
				}
				// A `replace` may redirect a module to a directory on the build
				// machine. What ships is the required path, never that.
				if strings.HasPrefix(m.Path, "/") || strings.HasPrefix(m.Path, ".") {
					t.Errorf("module %q is a filesystem path, not a module path", m.Path)
				}
			}
			if !found {
				t.Errorf("modules = %v, want one with path %q", payload.Modules, sampleDependency)
			}
			for _, m := range payload.Modules {
				if m.Path == sampleDependency {
					t.Logf("sample dependency: %s %s (of %d modules)", m.Path, m.Version, len(payload.Modules))
				}
			}
			// The sample imports net/http/pprof, so the marker must be found in
			// the binary the cluster is really running (ADR 0056 §1).
			if !payload.PprofEndpoint {
				t.Errorf("pprof_endpoint = false; the sample links net/http/pprof")
			}
			checkBuildSettings(t, payload.Settings)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("build payload for %s never appeared at %s", digest, path)
		}
		time.Sleep(5 * time.Second)
	}
}

// checkBuildSettings asserts what a real cluster shipped: nothing outside the
// allow-list, and the two settings the sample's Dockerfile actually fixes.
func checkBuildSettings(t *testing.T, settings map[string]string) {
	t.Helper()
	for key, value := range settings {
		if _, ok := allowedBuildSettings[key]; !ok {
			t.Errorf("build setting %q = %q left the cluster; only the allow-list may", key, value)
		}
	}
	// test/e2e/sample/Dockerfile builds with CGO_ENABLED=0, and GOARCH is
	// whatever the kind node is — the fact the node architecture is compared
	// against.
	if got := settings["CGO_ENABLED"]; got != "0" {
		t.Errorf("CGO_ENABLED = %q, want 0 (the sample is built static)", got)
	}
	if settings["GOARCH"] == "" {
		t.Error("GOARCH absent; the toolchain always records it")
	}
	// vcs.* is expected to be absent here: the sample's build context is
	// test/e2e/sample/, which holds no .git, so the toolchain has nothing to
	// stamp (ADR 0019). Logged rather than asserted — the point is that its
	// absence is normal, not that it is required.
	t.Logf("sample build settings: %v", settings)
}

// checkNodeArchitecture asserts that the node-metadata payload names every
// node's architecture. Without it a build's GOARCH has nothing to be compared
// against, which is the whole reason the settings above are collected.
func checkNodeArchitecture(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod string) {
	t.Helper()
	raw, ok := readSpoolFile(ctx, t, config, cs, ns, pod, nodeMetadataSpoolPath)
	if !ok {
		t.Errorf("node-metadata payload never appeared at %s", nodeMetadataSpoolPath)
		return
	}
	var payload struct {
		Kind  string `json:"kind"`
		Nodes []struct {
			Name         string `json:"name"`
			Architecture string `json:"architecture"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("node-metadata payload not valid JSON: %v", err)
	}
	if payload.Kind != "node_metadata" {
		t.Errorf("payload kind = %q, want node_metadata", payload.Kind)
	}
	if len(payload.Nodes) == 0 {
		t.Fatal("node-metadata payload carries no nodes")
	}
	for _, n := range payload.Nodes {
		if n.Architecture == "" {
			t.Errorf("node %q has no architecture", n.Name)
		}
	}
}

// readSpoolFile cats a payload file from the controller pod's spool via the
// shell sidecar. A non-zero exit (file not written yet) returns ok=false so the
// caller retries.
func readSpoolFile(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod, path string) (string, bool) {
	t.Helper()
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "spool-reader",
			Command:   []string{"cat", path},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		t.Fatalf("building exec: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		// cat exits non-zero until the controller writes the file — expected
		// during the poll, not a test failure.
		return "", false
	}
	return stdout.String(), true
}

// deploySampleWorkload creates the known Go workload as a Deployment and waits
// for its pod to run.
func deploySampleWorkload(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, image string) {
	t.Helper()
	replicas := int32(1)
	labels := map[string]string{"app": "goworkload"}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "goworkload"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "goworkload",
						Image:           image,
						ImagePullPolicy: corev1.PullNever,
					}},
				},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating sample deployment: %v", err)
	}
	waitDeploymentPod(ctx, t, cs, ns, "goworkload")
}

func mustUnmarshal(t *testing.T, doc []byte, into any) {
	t.Helper()
	if err := k8syaml.Unmarshal(doc, into); err != nil {
		t.Fatalf("decoding manifest doc: %v", err)
	}
}

func create(ctx context.Context, t *testing.T, kind string, fn func() error) {
	t.Helper()
	if err := fn(); err != nil {
		t.Fatalf("creating %s: %v", kind, err)
	}
}

// waitDeploymentPod waits for a Running pod of the named component (matched by
// the app label the Deployment sets) and returns its name.
func waitDeploymentPod(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name string) string {
	t.Helper()
	selectors := []string{
		"app.kubernetes.io/component=" + name,
		"app=" + name,
	}
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		for _, sel := range selectors {
			pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
			if err != nil {
				continue
			}
			for _, p := range pods.Items {
				if p.Status.Phase == corev1.PodRunning {
					return p.Name
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no Running pod for %q in %s before timeout", name, ns)
	return ""
}
