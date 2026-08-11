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

	"github.com/RebuildStackCo/runtime-agent/internal/nodeintake"
	"github.com/RebuildStackCo/runtime-agent/internal/nodeprofile"
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
	processWindow(context.Background(), logger, filter, 20, procRoot, "node",
		time.Unix(100, 0), time.Unix(160, 0), samples, targetSet, 5, 0, shipper)

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

func TestProcessWindowNoTargetsShipsNothing(t *testing.T) {
	procRoot := t.TempDir()
	writeCgroup(t, procRoot, 100, "1234abcd-12ab-34cd-56ef-1234567890ab", strings.Repeat("a", 64))
	samples := []nodeprofile.Sample{{PID: 100, Value: 1, Frames: []nodeprofile.Frame{{Function: "main.hot", Kind: "go"}}}}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	// empty target set -> nothing shipped, no shipper touched
	processWindow(context.Background(), logger, nodeprofile.NewSymbolFilter(nil, nodeprofile.ThirdPartyDrop),
		20, procRoot, "node", time.Unix(1, 0), time.Unix(2, 0), samples, map[string]struct{}{}, 5, 0, nil)
}
