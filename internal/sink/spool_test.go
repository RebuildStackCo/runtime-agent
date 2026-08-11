package sink

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/inventory"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// The golden files are the payload schema contract (docs/development.md): a
// change to these bytes is a protocol change and must be justified as one.
// Regenerate deliberately with: go test ./internal/sink -run Golden -update
var update = flag.Bool("update", false, "rewrite golden files")

var windowStart = time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

// fixedRecords builds a deterministic two-workload window through the real
// accumulator, exercising every field: CPU deltas, memory samples,
// throttling, and PSI.
func fixedRecords() []*rollup.Record {
	acc := rollup.NewAccumulator(time.Hour)
	web := rollup.Key{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"}
	idx := rollup.Key{Namespace: "search", WorkloadKind: "StatefulSet", WorkloadName: "index", Container: "app"}

	t0 := windowStart.Add(10 * time.Minute)
	t1 := t0.Add(30 * time.Second)
	t2 := t1.Add(30 * time.Second)

	acc.ObserveCPUDelta(web, t0, t1, 15e9)
	acc.ObserveCPUDelta(web, t1, t2, 6e9)
	acc.ObserveMemory(web, t1, 64<<20)
	acc.ObserveMemory(web, t2, 96<<20)
	acc.ObserveThrottling(web, t0, t1, 40, 100)
	acc.ObserveCPUPSI(web, t0, t1, 4e6)
	acc.ObserveMemoryPSI(web, t0, t1, 1e6)

	acc.ObserveCPUDelta(idx, t0, t2, 90e9)
	acc.ObserveMemory(idx, t2, 2<<30)

	return acc.Snapshots()
}

func checkGolden(t *testing.T, gotPath, goldenName string) {
	t.Helper()
	got, err := os.ReadFile(gotPath) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatalf("reading written payload: %v", err)
	}
	goldenPath := filepath.Join("testdata", goldenName)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o600); err != nil { // #nosec G703 -- test-controlled path
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("payload bytes differ from %s — this is a payload schema change; "+
			"if intended, justify it and regenerate with -update.\ngot:\n%s\nwant:\n%s",
			goldenPath, got, want)
	}
}

func newTestSpool(t *testing.T) (*Spool, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func snapshotFile(dir string) string {
	return filepath.Join(dir, fmt.Sprintf("usage-%d-3600.snapshot.json", windowStart.Unix()))
}

func closedFile(dir string) string {
	return filepath.Join(dir, fmt.Sprintf("usage-%d-3600.json", windowStart.Unix()))
}

func TestGoldenUsageSnapshotPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteUsageSnapshot(7, fixedRecords()); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, snapshotFile(dir), "usage-snapshot.golden.json")
}

func TestGoldenClosedWindowPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteClosedWindows(fixedRecords()); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, closedFile(dir), "usage-window.golden.json")
}

func TestGoldenOOMPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	event := collector.OOMKill{
		Namespace:        "shop",
		Pod:              "web-7f8d9-abcde",
		Container:        "app",
		Workload:         collector.WorkloadRef{Kind: "Deployment", Name: "web"},
		FinishedAt:       windowStart.Add(17 * time.Minute),
		ExitCode:         137,
		RestartCount:     3,
		MemoryLimitBytes: ptr.To(int64(128 << 20)),
	}
	if err := s.WriteOOMKill(event); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("oom-%d-shop-web-7f8d9-abcde-app-3.json", event.FinishedAt.Unix())
	checkGolden(t, filepath.Join(dir, name), "oom-kill.golden.json")
}

// fixedInventory is a deterministic two-workload Go inventory, already sorted by
// key as the store would return it.
func fixedInventory() []inventory.GoRecord {
	return []inventory.GoRecord{
		{
			Key:         inventory.Key{Namespace: "search", WorkloadKind: "StatefulSet", WorkloadName: "index", Container: "app"},
			GoVersion:   "go1.25.0",
			ModulePath:  "github.com/acme/index",
			ImageDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			PGO:         false,
		},
		{
			Key:         inventory.Key{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"},
			GoVersion:   "go1.26.1",
			ModulePath:  "github.com/acme/web",
			ImageDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			PGO:         true,
		},
	}
}

func TestGoldenGoInventoryPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteGoInventory(4, fixedInventory()); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, filepath.Join(dir, "go-inventory.json"), "go-inventory.golden.json")
}

func TestGoInventorySupersedesOnDisk(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteGoInventory(1, fixedInventory()); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteGoInventory(2, fixedInventory()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("spool holds %d files after two inventory writes, want 1 (supersede by key)", len(entries))
	}
	var payload struct {
		Sequence int64 `json:"sequence"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "go-inventory.json")) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Sequence != 2 {
		t.Fatalf("surviving inventory has sequence %d, want 2 (newest supersedes)", payload.Sequence)
	}
}

func TestGoldenProfilePayload(t *testing.T) {
	s, dir := newTestSpool(t)
	key := ProfileKey{
		Namespace:    "acme",
		Workload:     "api",
		Container:    "server",
		ImageDigest:  "sha256:deadbeef",
		CaptureStart: time.Unix(1_700_000_000, 0).UTC(),
		CaptureEnd:   time.Unix(1_700_000_060, 0).UTC(),
	}
	// A fixed byte fixture, not freshly serialized: gzip output is not stable
	// across toolchains, so the golden guards the payload wrapper, not gzip.
	pprofBytes := []byte("FIXED-PPROF-BYTES\x00\x01\x02\x03")
	if err := s.WriteProfile(key, pprofBytes); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, filepath.Join(dir, "profile-acme-api-server-1700000000-1700000060.json"), "profile.golden.json")
}

// TestProfilesDoNotSupersede is the counterpart to the inventory supersede test:
// each capture is its own window, so two captures of the same workload produce
// two files, never one (ADR 0011 §6 — no silent loss under rotation).
func TestProfilesDoNotSupersede(t *testing.T) {
	s, dir := newTestSpool(t)
	key := func(start int64) ProfileKey {
		return ProfileKey{
			Namespace: "n", Workload: "w", Container: "c",
			CaptureStart: time.Unix(start, 0).UTC(),
			CaptureEnd:   time.Unix(start+60, 0).UTC(),
		}
	}
	if err := s.WriteProfile(key(100), []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteProfile(key(200), []byte("b")); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "profile-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Errorf("got %d profile files, want 2 (captures do not supersede)", len(files))
	}
}

func TestSnapshotSupersedesOnDisk(t *testing.T) {
	s, dir := newTestSpool(t)
	records := fixedRecords()
	if err := s.WriteUsageSnapshot(1, records); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteUsageSnapshot(2, records); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("spool holds %d files after two snapshots of one window, want 1", len(entries))
	}
	var payload struct {
		Sequence int64 `json:"sequence"`
	}
	raw, err := os.ReadFile(snapshotFile(dir)) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Sequence != 2 {
		t.Fatalf("surviving snapshot has sequence %d, want 2 (newest supersedes)", payload.Sequence)
	}
}

func TestClosedWindowReplacesSnapshot(t *testing.T) {
	s, dir := newTestSpool(t)
	records := fixedRecords()
	if err := s.WriteUsageSnapshot(1, records); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteClosedWindows(records); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(snapshotFile(dir)); !os.IsNotExist(err) {
		t.Error("snapshot file survived the closed-window record that supersedes it")
	}
	if _, err := os.Stat(closedFile(dir)); err != nil {
		t.Errorf("closed-window file missing: %v", err)
	}
}

func TestSweepDropsExpiredAndTempFiles(t *testing.T) {
	s, dir := newTestSpool(t)
	old := filepath.Join(dir, "usage-100-3600.json")
	stale := filepath.Join(dir, "orphan.json.tmp")
	for _, p := range []string{old, stale} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	// The sweep rides the snapshot cadence.
	if err := s.WriteUsageSnapshot(1, fixedRecords()); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(old) || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("sweep kept expired file %s", e.Name())
		}
	}
	if _, err := os.Stat(snapshotFile(dir)); err != nil {
		t.Errorf("sweep removed the fresh snapshot: %v", err)
	}
}
