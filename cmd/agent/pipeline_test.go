package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/pprof/profile"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeintake"
	"github.com/RebuildStackCo/runtime-agent/internal/nodeprofile"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

func TestSamplesPerSecond(t *testing.T) {
	for ceiling, want := range map[int]int{5: 20, 0: 1, 100: 99, 3: 12} {
		if got := samplesPerSecond(ceiling); got != want {
			t.Errorf("samplesPerSecond(%d) = %d, want %d", ceiling, got, want)
		}
	}
}

func writeCgroup(t *testing.T, procRoot string, pid int, podUID, containerID string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	content := "0::/kubepods/burstable/pod" + podUID + "/" + containerID
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestProcessWindowShipsOnlyTargetedContainers is the core pipeline guard: a
// window's samples are grouped by container (resolved from each PID's cgroup),
// and only containers in the target set are filtered, serialized, validated, and
// shipped.
func TestProcessWindowShipsOnlyTargetedContainers(t *testing.T) {
	procRoot := t.TempDir()
	const podUID = "1234abcd-12ab-34cd-56ef-1234567890ab"
	cid1 := strings.Repeat("a", 64)
	cid2 := strings.Repeat("b", 64)
	writeCgroup(t, procRoot, 100, podUID, cid1) // targeted
	writeCgroup(t, procRoot, 200, podUID, cid2) // not targeted

	var mu sync.Mutex
	var shipped []nodeintake.ProfileReport
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep nodeintake.ProfileReport
		_ = json.NewDecoder(r.Body).Decode(&rep)
		mu.Lock()
		shipped = append(shipped, rep)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	shipper := newProfileShipper(srv.URL, writeToken(t, "tok"))

	svc := nodeprofile.Frame{Function: "main.hot", Kind: "go"} // survives the filter, so the profile validates
	samples := []nodeprofile.Sample{
		{PID: 100, Value: 3, Frames: []nodeprofile.Frame{svc}},
		{PID: 100, Value: 2, Frames: []nodeprofile.Frame{svc}},
		{PID: 200, Value: 9, Frames: []nodeprofile.Frame{svc}}, // cid2: not targeted -> dropped
	}
	filter := nodeprofile.NewSymbolFilter(nil, nodeprofile.ThirdPartyDrop)
	targetSet := map[string]struct{}{cid1: {}}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	processWindow(context.Background(), logger, staticFilter(filter), 20, procRoot, "node",
		time.Unix(100, 0), time.Unix(160, 0), samples, targetSet,
		nodescan.NewScope([]string{podUID}), shipper)

	mu.Lock()
	defer mu.Unlock()
	if len(shipped) != 1 {
		t.Fatalf("shipped %d profiles, want 1 (only the targeted container)", len(shipped))
	}
	if shipped[0].ContainerID != cid1 || shipped[0].PodUID != podUID {
		t.Errorf("shipped key = %s/%s, want %s/%s", shipped[0].PodUID, shipped[0].ContainerID, podUID, cid1)
	}
	if len(shipped[0].Pprof) == 0 {
		t.Error("shipped profile has no pprof bytes")
	}
}

// The defect ADR 0059 closes: with the configured allow-list empty — the chart's
// default — a service under its own domain-bearing module was redacted as
// third-party, and everything below `main` disappeared. The binary states its
// module, so the filter admits it without anything being configured.
func TestOwnCodeSurvivesWithNothingConfigured(t *testing.T) {
	const (
		podUID  = "1234abcd-12ab-34cd-56ef-1234567890ab"
		ownFunc = "github.com/acme/web/svc.Handle"
		depFunc = "github.com/vendor/lib.Do"
	)
	cid := strings.Repeat("a", 64)
	procRoot := t.TempDir()
	writeCgroup(t, procRoot, 100, podUID, cid)

	var mu sync.Mutex
	var shipped []nodeintake.ProfileReport
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep nodeintake.ProfileReport
		_ = json.NewDecoder(r.Body).Decode(&rep)
		mu.Lock()
		shipped = append(shipped, rep)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	samples := []nodeprofile.Sample{{PID: 100, Value: 5, Frames: []nodeprofile.Frame{
		{Function: ownFunc, Kind: "go"},
		{Function: depFunc, Kind: "go"},
	}}}

	index := &nodeprofile.ModuleIndex{}
	index.Publish(map[string][]string{cid: {"github.com/acme/web"}})
	// No configured prefixes at all: whatever survives, survives because the
	// scanner read the module off the binary.
	filterFor := func(containerID string) *nodeprofile.SymbolFilter {
		return nodeprofile.NewSymbolFilter(index.Modules(containerID), nodeprofile.ThirdPartyDrop)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	processWindow(context.Background(), logger, filterFor, 20, procRoot, "node",
		time.Unix(100, 0), time.Unix(160, 0), samples, map[string]struct{}{cid: {}},
		nodescan.NewScope([]string{podUID}), newProfileShipper(srv.URL, writeToken(t, "tok")))

	mu.Lock()
	defer mu.Unlock()
	if len(shipped) != 1 {
		t.Fatalf("shipped %d profiles, want 1 — the workload's own frame was redacted", len(shipped))
	}
	p, err := profile.ParseData(shipped[0].Pprof)
	if err != nil {
		t.Fatalf("shipped profile does not parse: %v", err)
	}
	names := map[string]bool{}
	for _, fn := range p.Function {
		names[fn.Name] = true
	}
	if !names[ownFunc] {
		t.Error("the workload's own frame did not survive; the build states its module")
	}
	if names[depFunc] {
		t.Error("a dependency frame survived under the drop policy")
	}
}

// And without the index the same window ships nothing, which is what the trap
// looked like: a profile of `main` and the runtime, rejected as unshippable.
func TestWithoutTheIndexTheSameWindowShipsNothing(t *testing.T) {
	const podUID = "1234abcd-12ab-34cd-56ef-1234567890ab"
	cid := strings.Repeat("a", 64)
	procRoot := t.TempDir()
	writeCgroup(t, procRoot, 100, podUID, cid)

	samples := []nodeprofile.Sample{{PID: 100, Value: 5, Frames: []nodeprofile.Frame{
		{Function: "github.com/acme/web/svc.Handle", Kind: "go"},
	}}}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	processWindow(context.Background(), logger, staticFilter(nil), 20, procRoot, "node",
		time.Unix(100, 0), time.Unix(160, 0), samples, map[string]struct{}{cid: {}},
		nodescan.NewScope([]string{podUID}), nil)
}

func TestProcessWindowNoTargetsShipsNothing(t *testing.T) {
	procRoot := t.TempDir()
	writeCgroup(t, procRoot, 100, "1234abcd-12ab-34cd-56ef-1234567890ab", strings.Repeat("a", 64))
	samples := []nodeprofile.Sample{{PID: 100, Value: 1, Frames: []nodeprofile.Frame{{Function: "main.hot", Kind: "go"}}}}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	// empty target set -> nothing shipped, no shipper touched
	processWindow(context.Background(), logger, staticFilter(nil),
		20, procRoot, "node", time.Unix(1, 0), time.Unix(2, 0), samples, map[string]struct{}{},
		nodescan.NewScope([]string{"1234abcd-12ab-34cd-56ef-1234567890ab"}), nil)
}

// A container the controller targeted but whose pod its own scan scope excludes
// is not profiled. The two answers come from the same controller, so this does
// not bound a hostile one — it catches a controller disagreeing with itself
// (informer lag) and removes the asymmetry where the more sensitive signal was
// taken from a pod the scanner may not even read (ADR 0025).
func TestProcessWindowShipsNothingOutsideTheScanScope(t *testing.T) {
	procRoot := t.TempDir()
	// The profiled pod is a different one from what the scope admits below —
	// the shape of a controller that named a container its own filters exclude.
	const excludedPodUID = "ffffffff-eeee-dddd-cccc-bbbbbbbbbbbb"
	cid := strings.Repeat("a", 64)
	writeCgroup(t, procRoot, 100, excludedPodUID, cid)

	shipped := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		shipped = true
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	samples := []nodeprofile.Sample{
		{PID: 100, Value: 3, Frames: []nodeprofile.Frame{{Function: "main.hot", Kind: "go"}}},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Targeted, but the scope admits a different pod.
	processWindow(context.Background(), logger, staticFilter(nil),
		20, procRoot, "node", time.Unix(100, 0), time.Unix(160, 0), samples,
		map[string]struct{}{cid: {}},
		nodescan.NewScope([]string{"1234abcd-12ab-34cd-56ef-1234567890ab"}),
		newProfileShipper(srv.URL, writeToken(t, "tok")))

	if shipped {
		t.Error("shipped a profile for a pod outside the controller's own scan scope")
	}
}

// And with no scope at all — an unreachable controller, or an endpoint the
// operator did not configure — the window ships nothing rather than everything
// it was told to. Same discipline as the scanner (ADR 0015 §2).
func TestProcessWindowFailsClosedWithoutAScope(t *testing.T) {
	procRoot := t.TempDir()
	const podUID = "1234abcd-12ab-34cd-56ef-1234567890ab"
	cid := strings.Repeat("a", 64)
	writeCgroup(t, procRoot, 100, podUID, cid)

	shipped := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		shipped = true
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	samples := []nodeprofile.Sample{
		{PID: 100, Value: 3, Frames: []nodeprofile.Frame{{Function: "main.hot", Kind: "go"}}},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	processWindow(context.Background(), logger, staticFilter(nil),
		20, procRoot, "node", time.Unix(100, 0), time.Unix(160, 0), samples,
		map[string]struct{}{cid: {}}, nodescan.DenyAll(),
		newProfileShipper(srv.URL, writeToken(t, "tok")))

	if shipped {
		t.Error("shipped a profile with no scan scope; profiling must fail closed")
	}
}

// staticFilter is the pre-ADR-0059 shape: one filter for every container. Tests
// that are not about which code is whose use it.
func staticFilter(f *nodeprofile.SymbolFilter) func(string) *nodeprofile.SymbolFilter {
	if f == nil {
		f = nodeprofile.NewSymbolFilter(nil, nodeprofile.ThirdPartyDrop)
	}
	return func(string) *nodeprofile.SymbolFilter { return f }
}
