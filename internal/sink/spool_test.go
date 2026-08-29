package sink

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/inventory"
	"github.com/RebuildStackCo/runtime-agent/internal/journal"
	"github.com/RebuildStackCo/runtime-agent/internal/metadata"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
	"github.com/RebuildStackCo/runtime-agent/internal/revisions"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// The golden files are the payload schema contract (docs/development.md): a
// change to these bytes is a protocol change and must be justified as one.
// Regenerate deliberately with: go test ./internal/sink -run Golden -update
var update = flag.Bool("update", false, "rewrite golden files")

var windowStart = time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

// capturedAt is the instant the structural snapshots below were taken. It is
// passed into the writers rather than read from the clock, which is what keeps
// the golden bytes stable.
var capturedAt = time.Date(2026, 8, 6, 10, 12, 0, 0, time.UTC)

// fixedCoverage is the completeness block of the Go-inventory payload: a fleet
// where one node has not checked in and a handful of facts could not be
// attributed — the state the block exists to make visible.
func fixedCoverage() inventory.Coverage {
	return inventory.Coverage{
		Since:           windowStart,
		NodesReported:   2,
		FactsReceived:   9,
		FactsJoined:     7,
		FactsUnjoined:   2,
		FactsUndigested: 1,
	}
}

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

// fixedObservation is a deterministic collection state: a cluster where some
// scrapes have failed and PSI is not exposed. The golden must show that a
// failed scrape is visible in the payload, not inferable only from logs.
// fixedNetwork is the finding in one payload: a log shipper on the host network
// moving two orders of magnitude more than the service it collects from — and
// flagged, because its counters are the node's, not its own.
func fixedNetwork() []*rollup.NetworkRecord {
	acc := rollup.NewNetworkAccumulator(time.Hour)
	t0 := windowStart.Add(10 * time.Minute)
	t1 := t0.Add(5 * time.Minute)

	acc.Observe(rollup.NetworkKey{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web"},
		t0, t1, rollup.NetworkDelta{RxBytes: 12 << 20, TxBytes: 48 << 20, Interfaces: 1}, false)
	acc.Observe(rollup.NetworkKey{Namespace: "observability", WorkloadKind: "DaemonSet", WorkloadName: "log-shipper"},
		t0, t1, rollup.NetworkDelta{RxBytes: 3 << 20, TxBytes: 4096 << 20, TxErrors: 17, Interfaces: 2}, true)

	return acc.CloseBefore(windowStart.Add(2 * time.Hour))
}

func TestGoldenNetworkWindowPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteNetworkWindows(fixedNetwork(), fixedObservation()); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("network-%d-3600.json", windowStart.Unix())
	checkGolden(t, filepath.Join(dir, name), "network-window.golden.json")
}

func fixedObservation() collector.Observation {
	return collector.Observation{
		PollIntervalSeconds: 30,
		PollsAttempted:      240,
		PollsFailed:         3,
		Signals:             []string{"cpu", "memory", "throttling"},
	}
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
	if err := s.WriteUsageSnapshot(fixedRecords(), fixedObservation()); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, snapshotFile(dir), "usage-snapshot.golden.json")
}

func TestGoldenClosedWindowPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteClosedWindows(fixedRecords(), fixedObservation()); err != nil {
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

// fixedRestarts is a deterministic restart window: one container whose reasons
// were all seen, and one restarting faster than the informer could keep up, so
// its breakdown deliberately sums to less than its total.
func fixedRestarts() []journal.RestartRecord {
	return []journal.RestartRecord{
		{
			Key:           journal.Key{Namespace: "search", Pod: "index-0", Container: "app"},
			Workload:      collector.WorkloadRef{Kind: "StatefulSet", Name: "index"},
			WindowStart:   windowStart,
			WindowSeconds: 3600,
			Restarts:      2,
			Reasons:       map[string]int64{"OOMKilled": 2},
			LastExitCode:  ptr.To(int32(137)),
		},
		{
			Key:               journal.Key{Namespace: "shop", Pod: "web-7f8d9-abcde", Container: "app"},
			Workload:          collector.WorkloadRef{Kind: "Deployment", Name: "web"},
			WindowStart:       windowStart,
			WindowSeconds:     3600,
			Restarts:          12,
			Reasons:           map[string]int64{"Error": 3, "other": 1},
			ReasonsUnobserved: 8,
			LastExitCode:      ptr.To(int32(1)),
		},
	}
}

func TestGoldenContainerRestartsPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteContainerRestarts(fixedRestarts()); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("restarts-%d-3600.json", windowStart.Unix())
	checkGolden(t, filepath.Join(dir, name), "container-restarts.golden.json")
}

// fixedRestartCounters is the pair the counter payload exists to tell apart.
//
// `index-0` is the day-one case: forty restarts already on the counter when the
// agent first looked, none of them in any window the agent will ever write.
// `web-7f8d9-abcde` is the ordinary one — the agent has watched every restart it
// has, so the reading and the windows agree. Nothing but this payload can state
// the difference.
func fixedRestartCounters() []collector.RestartCounter {
	observedSince := capturedAt.Add(-30 * time.Minute)
	return []collector.RestartCounter{
		{
			Namespace:                 "search",
			Pod:                       "index-0",
			Container:                 "app",
			Workload:                  collector.WorkloadRef{Kind: "StatefulSet", Name: "index"},
			Restarts:                  42,
			RestartsBeforeObservation: 40,
			ObservedSince:             observedSince,
			PodCreatedAt:              windowStart.Add(-72 * time.Hour),
			ContainerStartedAt:        ptr.To(capturedAt.Add(-3 * time.Minute)),
			LastTermination: &collector.RestartTermination{
				Reason:     "OOMKilled",
				ExitCode:   137,
				FinishedAt: capturedAt.Add(-3*time.Minute - time.Second),
			},
		},
		{
			Namespace:                 "shop",
			Pod:                       "web-7f8d9-abcde",
			Container:                 "app",
			Workload:                  collector.WorkloadRef{Kind: "Deployment", Name: "web"},
			Restarts:                  12,
			RestartsBeforeObservation: 0,
			ObservedSince:             observedSince,
			PodCreatedAt:              observedSince.Add(-5 * time.Minute),
			LastTermination: &collector.RestartTermination{
				Reason:     "Error",
				ExitCode:   1,
				FinishedAt: capturedAt.Add(-20 * time.Second),
			},
		},
	}
}

func TestGoldenRestartCountersPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteRestartCounters(capturedAt, fixedRestartCounters()); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, filepath.Join(dir, "restart-counters.json"), "restart-counters.golden.json")
}

// A cluster where nothing has ever restarted still writes the payload, with an
// empty record list. The journals stay silent in the same situation, and the
// difference is what the two shapes claim: a snapshot's empty list says "no
// container's counter is above zero", while an empty window would say only that
// the agent was running (ADR 0034).
func TestRestartCountersWriteEvenWhenNothingRestarted(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteRestartCounters(capturedAt, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "restart-counters.json")) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatalf("a cluster with no restarts must still write the reading: %v", err)
	}
	var payload struct {
		Records []collector.RestartCounter `json:"records"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Records == nil {
		t.Error("records must be an empty list, not null: absence is the claim being made")
	}
	if len(payload.Records) != 0 {
		t.Errorf("records = %d, want 0", len(payload.Records))
	}
}

// One file per window, not per restart: the crash loop must not decide how many
// files the spool holds (ADR 0020).
func TestContainerRestartsAreOneFilePerWindow(t *testing.T) {
	s, dir := newTestSpool(t)
	records := fixedRestarts()
	next := records[0]
	next.WindowStart = windowStart.Add(time.Hour)
	records = append(records, next)
	if err := s.WriteContainerRestarts(records); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("spool holds %d files for three records across two windows, want 2", len(entries))
	}
}

// A window's file supersedes: the newest write for a window replaces the
// previous one rather than accumulating, so the last write before the window
// closes is its final value.
func TestContainerRestartsSupersedeWithinTheirWindow(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteContainerRestarts(fixedRestarts()); err != nil {
		t.Fatal(err)
	}
	grown := fixedRestarts()
	grown[0].Restarts = 9
	if err := s.WriteContainerRestarts(grown); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("spool holds %d files after two writes of one window, want 1", len(entries))
	}
	var payload struct {
		Records []struct {
			Restarts int64 `json:"restarts"`
		} `json:"records"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("restarts-%d-3600.json", windowStart.Unix()))) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Records[0].Restarts != 9 {
		t.Errorf("surviving payload's first record has %d restarts, want 9 (newest supersedes)",
			payload.Records[0].Restarts)
	}
}

// fixedDisruptions is a deterministic disruption window: one pod preempted to
// make room and one evicted by a node under pressure — the two ways a cluster
// takes a workload away for capacity reasons.
func fixedDisruptions() []journal.DisruptionRecord {
	return []journal.DisruptionRecord{
		{
			Namespace:     "search",
			Pod:           "index-0",
			Workload:      collector.WorkloadRef{Kind: "StatefulSet", Name: "index"},
			Node:          "node-2",
			Reason:        "TerminationByKubelet",
			DisruptedAt:   windowStart.Add(9 * time.Minute),
			WindowStart:   windowStart,
			WindowSeconds: 3600,
		},
		{
			Namespace:     "shop",
			Pod:           "web-7f8d9-abcde",
			Workload:      collector.WorkloadRef{Kind: "Deployment", Name: "web"},
			Node:          "node-1",
			Reason:        "PreemptionByScheduler",
			DisruptedAt:   windowStart.Add(22 * time.Minute),
			WindowStart:   windowStart,
			WindowSeconds: 3600,
		},
	}
}

// fixedJobRuns is a deterministic window of finished runs: a CronJob run that
// succeeded after a retry, a CronJob run that exhausted its backoff limit, and
// a bare Job that never started before failing. The three cover the fields a
// consumer has to tell apart — a retry that cost resources, a failure reason
// that is a capacity fact, and a zero start instant that is a real state.
func fixedJobRuns() []journal.JobRunRecord {
	return []journal.JobRunRecord{
		{
			Namespace:     "analytics",
			Workload:      collector.WorkloadRef{Kind: "CronJob", Name: "rollup"},
			Name:          "rollup-29123456",
			StartedAt:     windowStart.Add(5 * time.Minute),
			FinishedAt:    windowStart.Add(7 * time.Minute),
			Result:        "succeeded",
			Succeeded:     1,
			Failed:        2,
			Parallelism:   ptrTo(int32(1)),
			Completions:   ptrTo(int32(1)),
			BackoffLimit:  ptrTo(int32(6)),
			WindowStart:   windowStart,
			WindowSeconds: 3600,
		},
		{
			Namespace:     "analytics",
			Workload:      collector.WorkloadRef{Kind: "CronJob", Name: "rollup"},
			Name:          "rollup-29123457",
			StartedAt:     windowStart.Add(35 * time.Minute),
			FinishedAt:    windowStart.Add(41 * time.Minute),
			Result:        "failed",
			FailReason:    "BackoffLimitExceeded",
			Failed:        7,
			Parallelism:   ptrTo(int32(1)),
			Completions:   ptrTo(int32(1)),
			BackoffLimit:  ptrTo(int32(6)),
			WindowStart:   windowStart,
			WindowSeconds: 3600,
		},
		{
			Namespace: "shop",
			Workload:  collector.WorkloadRef{Kind: "Job", Name: "migrate-v42"},
			Name:      "migrate-v42",
			// No StartedAt: the run failed before the controller recorded a
			// start, which the payload omits rather than rendering as the epoch.
			FinishedAt:    windowStart.Add(12 * time.Minute),
			Result:        "failed",
			FailReason:    "DeadlineExceeded",
			WindowStart:   windowStart,
			WindowSeconds: 3600,
		},
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestGoldenJobRunsPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteJobRuns(fixedJobRuns()); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("job-runs-%d-3600.json", windowStart.Unix())
	checkGolden(t, filepath.Join(dir, name), "job-runs.golden.json")
}

// One file per window, not per run. A cluster whose CronJobs finish hundreds of
// runs an hour must not put the spool's file count under its own control
// (ADR 0029, the reasoning ADR 0021 established for disruptions).
func TestJobRunsOfOneWindowShareAFile(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteJobRuns(fixedJobRuns()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("spool holds %d files for three runs of one window, want 1", len(entries))
	}
}

func TestGoldenPodDisruptionsPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WritePodDisruptions(fixedDisruptions()); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("disruptions-%d-3600.json", windowStart.Unix())
	checkGolden(t, filepath.Join(dir, name), "pod-disruptions.golden.json")
}

// A node evicting many pods at once must not write one file per pod: the
// payload's file count is bounded by time, not by how bad the incident is
// (ADR 0021).
func TestPodDisruptionsAreOneFilePerWindow(t *testing.T) {
	s, dir := newTestSpool(t)
	records := fixedDisruptions()
	for i := range 8 {
		extra := records[0]
		extra.Pod = fmt.Sprintf("index-%d", i+1)
		records = append(records, extra)
	}
	if err := s.WritePodDisruptions(records); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("spool holds %d files for ten disruptions in one window, want 1", len(entries))
	}
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
	if err := s.WriteGoInventory(capturedAt, fixedCoverage(), fixedInventory()); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, filepath.Join(dir, "go-inventory.json"), "go-inventory.golden.json")
}

// The peaks fixture is the finding in one payload: `web/app` has run its build
// to within a hair of a limit, across two replicas that see different CPU
// counts, and `db` has not.
func fixedPeaks() []inventory.PeakRecord {
	return []inventory.PeakRecord{
		{
			PeakKey: inventory.PeakKey{
				Key: inventory.Key{
					Namespace: "shop", WorkloadKind: "Deployment",
					WorkloadName: "web", Container: "app",
				},
				ImageDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			},
			PeakRSSBytes: 481 << 20, Processes: 2,
			CPUsAllowedMin: 4, CPUsAllowedMax: 8,
		},
		{
			PeakKey: inventory.PeakKey{
				Key: inventory.Key{
					Namespace: "shop", WorkloadKind: "StatefulSet",
					WorkloadName: "db", Container: "db",
				},
			},
			PeakRSSBytes: 96 << 20, Processes: 1,
			CPUsAllowedMin: 8, CPUsAllowedMax: 8,
		},
	}
}

func TestGoldenProcessPeaksPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteProcessPeaks(capturedAt, fixedPeaks()); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, filepath.Join(dir, "process-peaks.json"), "process-peaks.golden.json")
}

func TestGoInventorySupersedesOnDisk(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteGoInventory(capturedAt, fixedCoverage(), fixedInventory()); err != nil {
		t.Fatal(err)
	}
	later := capturedAt.Add(time.Minute)
	if err := s.WriteGoInventory(later, fixedCoverage(), fixedInventory()); err != nil {
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
		CapturedAt time.Time `json:"captured_at"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "go-inventory.json")) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.CapturedAt.Equal(later) {
		t.Fatalf("surviving inventory was captured at %s, want %s (newest supersedes)",
			payload.CapturedAt, later)
	}
}

func fixedBuild() inventory.BuildFacts {
	return inventory.BuildFacts{
		ImageDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		GoVersion:   "go1.26.1",
		MainModule:  "github.com/acme/web",
		// One module replaced, so the golden shows the flag and shows that the
		// version reported is the required one and not the replacement's
		// (ADR 0048 §3).
		Modules: []nodescan.Module{
			{Path: "github.com/cespare/xxhash/v2", Version: "v2.3.0"},
			{Path: "go.uber.org/automaxprocs", Version: "v1.6.0"},
			{Path: "golang.org/x/sync", Version: "v0.9.0", Replaced: true},
		},
		Settings: map[string]string{
			"CGO_ENABLED":  "0",
			"GOAMD64":      "v1",
			"GOARCH":       "amd64",
			"vcs":          "git",
			"vcs.modified": "false",
			"vcs.revision": "a5edd4b28e4f6d042bb29b6fe5f8c7970a0f6485",
			"vcs.time":     "2026-08-21T20:13:54Z",
		},
		// A go1.26 toolchain over a main module still on an older `go`
		// directive, which is the only combination that puts these two in the
		// payload at all (ADR 0050 §2).
		GoDebug: map[string]string{
			"containermaxprocs": "0",
			"updatemaxprocs":    "0",
		},
	}
}

func TestGoldenGoBuildPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteGoBuild(fixedBuild()); err != nil {
		t.Fatal(err)
	}
	name := "go-build-sha256-2222222222222222222222222222222222222222222222222222222222222222.json"
	checkGolden(t, filepath.Join(dir, name), "go-build.golden.json")
}

// A build's facts never change, so writing them twice must land on the same
// file with the same bytes: redelivery after a restart is an idempotent
// upsert, which is what makes the controller's in-memory "already written"
// bookkeeping loss-harmless (ADR 0017).
func TestGoBuildWriteIsIdempotent(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteGoBuild(fixedBuild()); err != nil {
		t.Fatal(err)
	}
	name := "go-build-sha256-2222222222222222222222222222222222222222222222222222222222222222.json"
	first, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteGoBuild(fixedBuild()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("spool holds %d files after two writes of one build, want 1", len(entries))
	}
	second, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("the same build produced different bytes on rewrite; the payload must be immutable given its digest")
	}
}

// Two builds are two files: unlike the inventory snapshot, build facts
// accumulate rather than supersede, because each set is keyed by its own build.
func TestGoBuildsAccumulatePerBuild(t *testing.T) {
	s, dir := newTestSpool(t)
	first := fixedBuild()
	second := fixedBuild()
	second.ImageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	for _, d := range []inventory.BuildFacts{first, second} {
		if err := s.WriteGoBuild(d); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("spool holds %d files for two builds, want 2", len(entries))
	}
}

// Build facts with no digest have no key, so they cannot be filed at all —
// writing them would produce a payload nothing can join to.
func TestGoBuildRejectsMissingDigest(t *testing.T) {
	s, dir := newTestSpool(t)
	d := fixedBuild()
	d.ImageDigest = ""
	if err := s.WriteGoBuild(d); err == nil {
		t.Fatal("writing build facts with no image digest succeeded, want an error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("spool holds %d files after a rejected write, want 0", len(entries))
	}
}

// fixedWorkloadMetadata is a deterministic metadata snapshot, already sorted by
// key as metadata.Aggregate returns it, covering the three shapes the payload
// has to express at once: a workload mid-rollout (two builds of "app" with
// different requests), a build with no limits declared, and a meshed pod whose
// sidecar is a second record repeating the same pod facts.
//
// The workload block repeats on every record for the same reason the pod block
// does, and the golden is what shows it (ADR 0014).
func fixedWorkloadMetadata() []metadata.Record {
	// A strategy that replaces a quarter of the fleet at a time and waits no
	// time at all before calling a new replica available (ADR 0048 §2).
	rolling := metadata.WorkloadScope{UpdateStrategy: collector.UpdateStrategy{
		Type:           "RollingUpdate",
		MaxUnavailable: "25%",
		MaxSurge:       "25%",
	}}
	running := metadata.PodScope{
		QOSClass: "Burstable",
		Replicas: 2,
		Phases:   map[string]int{"Running": 2},
		Nodes:    map[string]int{"node-1": 1, "node-2": 1},
		// A workload that cannot be packed: required anti-affinity on hostname
		// puts one replica per node whatever spare capacity exists elsewhere,
		// and DoNotSchedule across zones keeps it paying for every zone it
		// spans (ADR 0031).
		Placement: collector.Placement{
			NodeSelector: map[string]string{"node.kubernetes.io/instance-type": "m6i.large"},
			PodAntiAffinity: []collector.TopologyTerm{
				{TopologyKey: "kubernetes.io/hostname", Required: true},
			},
			TopologySpread: []collector.SpreadTerm{
				{TopologyKey: "topology.kubernetes.io/zone", MaxSkew: 1, WhenUnsatisfiable: "DoNotSchedule"},
			},
			PriorityClass:           "high",
			TerminationGraceSeconds: ptr.To[int64](300),
		},
	}
	return []metadata.Record{
		{
			Key: metadata.Key{
				Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web",
				Container:   "app",
				ImageDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			},
			Image: "example.com/web:1.2.3",
			Resources: collector.Resources{
				CPURequestMilli:    ptr.To[int64](500),
				CPULimitMilli:      ptr.To[int64](2000),
				MemoryRequestBytes: ptr.To[int64](256 << 20),
				MemoryLimitBytes:   ptr.To[int64](1 << 30),
			},
			Ports: []collector.ContainerPort{{Name: "http", Port: 8080, Protocol: "TCP"}},
			// A liveness probe that gives up sooner than the readiness probe
			// does — three failures at ten seconds against six at five — is a
			// restart on a timer. The payload carries the two schedules and
			// nothing about what either checks (ADR 0048 §1).
			Probes: collector.Probes{
				Liveness: &collector.Probe{
					Kind: "httpGet", PeriodSeconds: 10, TimeoutSeconds: 1,
					FailureThreshold: 3, SuccessThreshold: 1,
				},
				Readiness: &collector.Probe{
					Kind: "httpGet", InitialDelaySeconds: 5, PeriodSeconds: 5,
					TimeoutSeconds: 1, FailureThreshold: 6, SuccessThreshold: 1,
				},
			},
			Pod:      running,
			Workload: rolling,
		},
		{
			Key: metadata.Key{
				Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web",
				Container:   "app",
				ImageDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			},
			Image: "example.com/web:1.3.0",
			// No limits declared: nil must stay absent from the payload, never
			// flatten to zero.
			Resources: collector.Resources{CPURequestMilli: ptr.To[int64](1000)},
			// One knob written inline and one derived from the container's own
			// limits, which is the form the downward API gives it: the spec
			// holds no value, so the payload names the field it reads
			// (ADR 0047). The sidecar below sets neither, and its record must
			// carry no `runtime_env` at all.
			RuntimeEnv: map[string]string{
				"GOMAXPROCS": "resource:limits.cpu",
				"GOMEMLIMIT": "900MiB",
			},
			// The new build's replica is Pending with no node — and the reason
			// is in the payload, so the shortfall between Replicas and Nodes is
			// explained rather than merely visible (ADR 0021).
			Pod: metadata.PodScope{
				QOSClass:    "Burstable",
				Replicas:    1,
				Phases:      map[string]int{"Pending": 1},
				Unscheduled: map[string]int{"Unschedulable": 1},
				// The new build tightened its node affinity, and its replica is
				// unschedulable. Because the record key carries the image
				// digest, the two builds keep their own constraints instead of
				// one overwriting the other — which is the whole reason a
				// rollout's placement change is legible here at all.
				Placement: collector.Placement{
					NodeAffinity: []collector.NodeAffinityTerm{{
						Key:      "topology.kubernetes.io/zone",
						Operator: "In",
						Values:   []string{"eu-west-1a"},
						Required: true,
					}},
					PodAntiAffinity: []collector.TopologyTerm{
						{TopologyKey: "kubernetes.io/hostname", Required: true},
					},
					PriorityClass: "high",
				},
			},
			Workload: rolling,
		},
		{
			// The sidecar of the same pods. Its pod block is identical to the
			// app container's — the pods are the same pods, counted once. The
			// nesting is what makes summing these across containers obviously
			// wrong instead of merely wrong.
			Key: metadata.Key{
				Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web",
				Container:   "istio-proxy",
				ImageDigest: "sha256:3333333333333333333333333333333333333333333333333333333333333333",
			},
			Image:     "docker.io/istio/proxyv2:1.24.0",
			Resources: collector.Resources{CPURequestMilli: ptr.To[int64](100)},
			Pod:       running,
			Workload:  rolling,
		},
		{
			// A workload whose kind the agent does not read — here an Argo
			// Rollout. The golden carries it to pin the shape: no `workload`
			// block at all, and no field standing in for one (ADR 0048 §2).
			Key: metadata.Key{
				Namespace: "shop", WorkloadKind: "Rollout", WorkloadName: "payments",
				Container:   "app",
				ImageDigest: "sha256:4444444444444444444444444444444444444444444444444444444444444444",
			},
			Image:     "example.com/payments:2.0.1",
			Resources: collector.Resources{CPURequestMilli: ptr.To[int64](250)},
			Pod: metadata.PodScope{
				QOSClass: "Burstable",
				Replicas: 1,
				Phases:   map[string]int{"Running": 1},
				Nodes:    map[string]int{"node-1": 1},
			},
		},
	}
}

// fixedNodeMetadata is a mixed-architecture fleet on purpose: the architecture
// field exists so a build's GOARCH has something to be compared against, and a
// golden where every node is the same architecture would not show that.
func fixedNodeMetadata() []collector.NodeInfo {
	return []collector.NodeInfo{
		{
			Name: "node-1", InstanceType: "m6i.large", CapacityType: "on-demand",
			Zone: "eu-west-1a", Region: "eu-west-1", KernelVersion: "6.1.0-generic",
			Architecture:        "amd64",
			AllocatableCPUMilli: 1930, AllocatableMemoryBytes: 7 << 30,
			CapacityCPUMilli: 2000, CapacityMemoryBytes: 8 << 30,
		},
		{
			Name: "node-2", InstanceType: "m7g.large", CapacityType: "spot",
			Zone: "eu-west-1b", Region: "eu-west-1", KernelVersion: "6.1.0-generic",
			Architecture:        "arm64",
			AllocatableCPUMilli: 1930, AllocatableMemoryBytes: 7 << 30,
			CapacityCPUMilli: 2000, CapacityMemoryBytes: 8 << 30,
		},
	}
}

func TestGoldenWorkloadMetadataPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteWorkloadMetadata(capturedAt, fixedWorkloadMetadata()); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, filepath.Join(dir, "workload-metadata.json"), "workload-metadata.golden.json")
}

func TestGoldenNodeMetadataPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteNodeMetadata(capturedAt, fixedNodeMetadata()); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, filepath.Join(dir, "node-metadata.json"), "node-metadata.golden.json")
}

// Metadata describes current state, so each flush replaces its predecessor
// rather than accumulating — the on-disk mirror of upsert-by-key ingest.
// Each flush carries a later capture instant, and the last one is what the file
// holds. That is also the whole of the ordering story since ADR 0027: the spool
// keeps one version of a key, so "which is newest" is answered by which file is
// there, not by a counter inside it.
func TestMetadataSupersedesOnDisk(t *testing.T) {
	s, dir := newTestSpool(t)
	var last time.Time
	for i := range 3 {
		last = capturedAt.Add(time.Duration(i) * time.Minute)
		if err := s.WriteWorkloadMetadata(last, fixedWorkloadMetadata()); err != nil {
			t.Fatal(err)
		}
		if err := s.WriteNodeMetadata(last, fixedNodeMetadata()); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("spool holds %d files after three flushes, want 2 (supersede by key)", len(entries))
	}
	for _, name := range []string{"workload-metadata.json", "node-metadata.json"} {
		var payload struct {
			Kind       string    `json:"kind"`
			Source     string    `json:"source"`
			CapturedAt time.Time `json:"captured_at"`
		}
		raw, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- test-controlled path
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		if !payload.CapturedAt.Equal(last) {
			t.Errorf("%s was captured at %s, want %s (newest supersedes)",
				name, payload.CapturedAt, last)
		}
		// Provenance is what stops the backend merging a declared value with a
		// measured or sampled one under the same key.
		if payload.Source != SourceStructural {
			t.Errorf("%s source = %q, want %q", name, payload.Source, SourceStructural)
		}
	}
}

// fixedRevisions is a Deployment mid-rollout: the outgoing revision still
// carrying traffic, the incoming one not yet ready, and an older one kept at
// zero. The three are what a consumer must be able to tell apart, and none of
// those states is named by the agent.
// fixedRevisions is a Deployment mid-rollout — three numbered revisions, the
// newest not yet ready — plus a workload whose controller is a custom resource.
// The second carries no revision number, which is the shape ADR 0049 §3 chose
// over learning another controller's annotation key.
func fixedRevisions() []revisions.Record {
	return []revisions.Record{
		{
			Namespace: "shop",
			Workload:  collector.WorkloadRef{Kind: "Deployment", Name: "web"},
			Name:      "web-6d4cf56db",
			Revision:  ptrTo(int64(46)),
			CreatedAt: capturedAt.Add(-72 * time.Hour),
			Replicas:  revisions.Replicas{Desired: 0, Current: 0, Ready: 0},
			Containers: []revisions.Container{
				{Name: "app", Image: "example.com/app:1.2.2"},
			},
		},
		{
			Namespace: "shop",
			Workload:  collector.WorkloadRef{Kind: "Deployment", Name: "web"},
			Name:      "web-7f8d9c5b4",
			Revision:  ptrTo(int64(47)),
			CreatedAt: capturedAt.Add(-2 * time.Hour),
			Replicas:  revisions.Replicas{Desired: 3, Current: 3, Ready: 3},
			Containers: []revisions.Container{
				{Name: "migrate", Image: "example.com/migrate:v3", Init: true},
				{Name: "app", Image: "example.com/app:1.2.3"},
			},
		},
		{
			Namespace: "shop",
			Workload:  collector.WorkloadRef{Kind: "Deployment", Name: "web"},
			Name:      "web-849fbc77d",
			Revision:  ptrTo(int64(48)),
			CreatedAt: capturedAt.Add(-3 * time.Minute),
			Replicas:  revisions.Replicas{Desired: 1, Current: 1, Ready: 0},
			Containers: []revisions.Container{
				{Name: "migrate", Image: "example.com/migrate:v3", Init: true},
				{Name: "app", Image: "example.com/app:1.2.4"},
			},
		},
		{
			Namespace: "shop",
			Workload:  collector.WorkloadRef{Kind: "Rollout", Name: "payments"},
			Name:      "payments-7bd58f4db4",
			CreatedAt: capturedAt.Add(-20 * time.Minute),
			Replicas:  revisions.Replicas{Desired: 1, Current: 1, Ready: 1},
			Containers: []revisions.Container{
				{Name: "app", Image: "example.com/payments:2.0.1"},
			},
		},
	}
}

func TestGoldenWorkloadRevisionsPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteWorkloadRevisions(capturedAt, fixedRevisions()); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, filepath.Join(dir, "workload-revisions.json"), "workload-revisions.golden.json")
}

// Current state under a fixed key, so each flush replaces its predecessor
// rather than accumulating — the on-disk mirror of upsert-by-key ingest.
func TestWorkloadRevisionsSupersedeOnDisk(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteWorkloadRevisions(capturedAt, fixedRevisions()); err != nil {
		t.Fatal(err)
	}
	later := capturedAt.Add(time.Minute)
	if err := s.WriteWorkloadRevisions(later, fixedRevisions()[:1]); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("spool holds %d files after two revision writes, want 1 (supersede by key)", len(entries))
	}
	var payload struct {
		CapturedAt time.Time `json:"captured_at"`
		Records    []struct {
			Name string `json:"name"`
		} `json:"records"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "workload-revisions.json")) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.CapturedAt.Equal(later) || len(payload.Records) != 1 {
		t.Errorf("surviving payload = (%s, %d records), want (%s, 1); a superseding snapshot "+
			"is the complete current truth, not a merge with its predecessor",
			payload.CapturedAt, len(payload.Records), later)
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
	checkGolden(t, filepath.Join(dir, "profile-acme-api-server-deadbeef-1700000000-1700000060.json"), "profile.golden.json")
}

// The case the old filename could not express, and the one that actually
// happens: the node cuts every window on the same boundaries and ships one
// report per container, so two replicas of a workload on one node — or two
// builds mid-rollout — arrive with identical namespace, workload, container and
// capture interval. Only the digest separates them (ADR 0023).
func TestProfilesOfOneWindowAreKeyedByDigest(t *testing.T) {
	s, dir := newTestSpool(t)
	key := func(digest string) ProfileKey {
		return ProfileKey{
			Namespace: "n", Workload: "w", Container: "c",
			ImageDigest:  digest,
			CaptureStart: time.Unix(100, 0).UTC(),
			CaptureEnd:   time.Unix(160, 0).UTC(),
		}
	}
	if err := s.WriteProfile(key("sha256:1111111111111111"), []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteProfile(key("sha256:2222222222222222"), []byte("b")); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "profile-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d profile files, want 2 — one build's capture replaced the other's", len(files))
	}
}

// A capture the controller could not tie to a build still has to land, and it
// must not collide with one that could. It cannot be dropped: the samples are
// real and the missing digest is the controller's informer lagging, not the
// container failing to start.
func TestProfileWithoutADigestIsNamedForThatState(t *testing.T) {
	s, dir := newTestSpool(t)
	key := func(digest string) ProfileKey {
		return ProfileKey{
			Namespace: "n", Workload: "w", Container: "c",
			ImageDigest:  digest,
			CaptureStart: time.Unix(100, 0).UTC(),
			CaptureEnd:   time.Unix(160, 0).UTC(),
		}
	}
	if err := s.WriteProfile(key(""), []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteProfile(key("sha256:abcdef123456789"), []byte("b")); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "profile-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d profile files, want 2", len(files))
	}
	if _, err := os.Stat(filepath.Join(dir, "profile-n-w-c-nodigest-100-160.json")); err != nil {
		t.Errorf("the undigested capture is not named for that state: %v", err)
	}
}

// A digest reaches the filesystem as a name, so nothing a registry could put in
// one may become a path.
func TestShortDigestYieldsOneSafeSegment(t *testing.T) {
	cases := []struct{ digest, want string }{
		{"sha256:deadbeef", "deadbeef"},
		{"sha256:0123456789abcdef0123456789abcdef", "0123456789ab"},
		{"", noDigest},
		{"sha512:../../etc/passwd", "ecad"}, // only hex survives; no separator can
		{"sha256:ZZZZ", noDigest},
	}
	for _, c := range cases {
		if got := shortDigest(c.digest); got != c.want {
			t.Errorf("shortDigest(%q) = %q, want %q", c.digest, got, c.want)
		}
	}
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
	if err := s.WriteUsageSnapshot(records, fixedObservation()); err != nil {
		t.Fatal(err)
	}
	// A later snapshot of the same window has polled more times — the observable
	// difference between two writes now that no counter labels them.
	later := fixedObservation()
	later.PollsAttempted++
	if err := s.WriteUsageSnapshot(records, later); err != nil {
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
		Observation collector.Observation `json:"observation"`
	}
	raw, err := os.ReadFile(snapshotFile(dir)) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Observation.PollsAttempted != later.PollsAttempted {
		t.Fatalf("surviving snapshot reports %d polls attempted, want %d (newest supersedes)",
			payload.Observation.PollsAttempted, later.PollsAttempted)
	}
}

func TestClosedWindowReplacesSnapshot(t *testing.T) {
	s, dir := newTestSpool(t)
	records := fixedRecords()
	if err := s.WriteUsageSnapshot(records, fixedObservation()); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteClosedWindows(records, fixedObservation()); err != nil {
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

	if err := s.WriteUsageSnapshot(fixedRecords(), fixedObservation()); err != nil {
		t.Fatal(err)
	}
	// Sweeping is the agent's own periodic call, not a side effect of writing
	// a snapshot (ADR 0042).
	if err := s.Sweep(time.Now()); err != nil {
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

// fixedWorkloadPolicy is a workload held in place by every mechanism at once: a
// budget that permits no disruption today, an autoscaler pinned at its ceiling,
// a bound zonal claim, and a Service routing to it. Each alone changes what may
// be concluded from the same usage numbers.
func fixedWorkloadPolicy() []collector.WorkloadPolicy {
	return []collector.WorkloadPolicy{
		{
			Namespace: "shop",
			Workload:  collector.WorkloadRef{Kind: "Deployment", Name: "web"},
			Budgets: []collector.DisruptionBudget{{
				Name: "web-pdb", MinAvailable: "100%",
				DisruptionsAllowed: 0, CurrentHealthy: 3, DesiredHealthy: 3, ExpectedPods: 3,
			}},
			Autoscalers: []collector.Autoscaler{{
				Name:        "web-hpa",
				MinReplicas: ptr.To[int32](3),
				MaxReplicas: 8, CurrentReplicas: 8, DesiredReplicas: 8,
				Metrics: []collector.AutoscalerMetric{{
					Type: "Resource", Name: "cpu",
					TargetType: "Utilization", TargetValue: "80",
				}},
				LimitedReason: "TooManyReplicas",
			}},
			// Topology-aware routing asked for and not arranged: the mode is
			// set, and not one endpoint carries a hint (ADR 0051).
			Services: []collector.ServiceExposure{{
				Name: "web", Type: "ClusterIP", InternalTrafficPolicy: "Cluster",
				TrafficDistribution: "PreferClose",
				Endpoints: []collector.EndpointZones{{
					AddressType: "IPv4", Ready: 3,
					Zones: map[string]int{"eu-west-1a": 2, "eu-west-1b": 1},
				}},
			}},
		},
		{
			Namespace: "shop",
			Workload:  collector.WorkloadRef{Kind: "StatefulSet", Name: "db"},
			Claims: []collector.VolumeClaim{{
				Name: "data-db-0", StorageClass: "gp3-zonal",
				AccessModes: []string{"ReadWriteOnce"}, RequestedBytes: 100 << 30, Phase: "Bound",
			}},
			// A headless Service: clients resolve the pods themselves, so a
			// single replica is a single address with nothing in front of it.
			Services: []collector.ServiceExposure{{
				Name: "db", Type: "ClusterIP", Headless: true,
			}},
		},
	}
}

// fixedClusterPolicy pairs a namespace that supplies defaults with the catalogs
// the workload payloads point into by name.
func fixedClusterPolicy() collector.ClusterPolicy {
	return collector.ClusterPolicy{
		Namespaces: []collector.NamespacePolicy{{
			Namespace: "shop",
			LimitRanges: []collector.LimitRangeInfo{{
				Name: "defaults",
				Items: []collector.LimitRangeItem{{
					Type:           "Container",
					DefaultRequest: collector.ResourceAmounts{CPUMilli: ptr.To[int64](100)},
					Default:        collector.ResourceAmounts{MemoryBytes: ptr.To[int64](512 << 20)},
					Max:            collector.ResourceAmounts{CPUMilli: ptr.To[int64](4000)},
				}},
			}},
			Quotas: []collector.ResourceQuotaInfo{{
				Name: "team",
				Hard: map[string]string{"requests.cpu": "40", "requests.memory": "80Gi"},
				Used: map[string]string{"requests.cpu": "31", "requests.memory": "62Gi"},
			}},
		}},
		PriorityClasses: []collector.PriorityClassInfo{
			{Name: "high", Value: 1000, PreemptionPolicy: "PreemptLowerPriority"},
			{Name: "low", Value: 100, PreemptionPolicy: "Never"},
		},
		StorageClasses: []collector.StorageClassInfo{{
			Name: "gp3-zonal", Provisioner: "ebs.csi.aws.com",
			ReclaimPolicy: "Delete", VolumeBindingMode: "WaitForFirstConsumer",
			AllowVolumeExpansion: true,
		}},
	}
}

func TestGoldenWorkloadPolicyPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteWorkloadPolicy(capturedAt, fixedWorkloadPolicy(), nil); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, filepath.Join(dir, "workload-policy.json"), "workload-policy.golden.json")
}

func TestGoldenClusterPolicyPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteClusterPolicy(capturedAt, fixedClusterPolicy(), nil); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, filepath.Join(dir, "cluster-policy.json"), "cluster-policy.golden.json")
}

// Both policy payloads supersede under a fixed key: a second write replaces the
// first rather than accumulating a second version to rank (ADR 0027).
func TestPolicySupersedesOnDisk(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteWorkloadPolicy(capturedAt, fixedWorkloadPolicy(), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteWorkloadPolicy(capturedAt.Add(time.Minute), nil, nil); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "workload-policy.json" {
		t.Fatalf("spool holds %d files, want one superseding payload", len(entries))
	}
}

// The declared-source line is behavior rather than shape: the field is a string
// list, and both payloads keep their golden showing the healthy case so every
// rich shape stays pinned. What matters is that the line is written, that each
// payload declares only its own sources, and that a healthy capture writes
// nothing — because empty is itself the statement "every source was read".
func TestUnavailableSourcesAreDeclaredPerPayload(t *testing.T) {
	s, dir := newTestSpool(t)
	if err := s.WriteWorkloadPolicy(capturedAt, fixedWorkloadPolicy(), []string{"pod_disruption_budgets"}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteClusterPolicy(capturedAt, fixedClusterPolicy(), nil); err != nil {
		t.Fatal(err)
	}

	var workload struct {
		UnavailableSources []string `json:"unavailable_sources"`
	}
	decodeSpooled(t, filepath.Join(dir, "workload-policy.json"), &workload)
	if len(workload.UnavailableSources) != 1 || workload.UnavailableSources[0] != "pod_disruption_budgets" {
		t.Errorf("workload policy sources = %v", workload.UnavailableSources)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "cluster-policy.json")) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "unavailable_sources") {
		t.Errorf("a healthy capture wrote a sources line:\n%s", raw)
	}
}

func decodeSpooled(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

// TestAnOwnerReferenceNameCannotEscapeTheSpool is the traversal this slice
// closes (ADR 0042).
//
// A profile's filename is built from the workload name, taken from
// `ownerReferences[].name`, which the API server validates only for
// non-emptiness — so a crafted owner reference put an arbitrary path into a name
// filepath.Join then resolved. readOnlyRootFilesystem made most targets fail
// with EROFS, which is luck rather than design.
func TestAnOwnerReferenceNameCannotEscapeTheSpool(t *testing.T) {
	hostile := []string{
		"../../../../tmp/pwn",
		"x/../../../../etc/cron.d/evil",
		"..",
		".",
		"../",
		`..\..\windows`,
		"a/b",
		"",
	}

	for _, name := range hostile {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			s, dir := newTestSpool(t)
			key := ProfileKey{
				Namespace:    "acme",
				Workload:     name,
				Container:    "server",
				ImageDigest:  "sha256:deadbeef",
				CaptureStart: time.Unix(100, 0).UTC(),
				CaptureEnd:   time.Unix(160, 0).UTC(),
			}
			if err := s.WriteProfile(key, []byte{1, 2, 3}); err != nil {
				t.Fatalf("writing a profile for a hostile workload name failed: %v", err)
			}

			// Every file the write produced must be directly inside the spool.
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("wrote %d entries, want exactly 1 inside the spool", len(entries))
			}
			got := entries[0].Name()
			if strings.ContainsAny(got, `/\`) || got == "." || got == ".." {
				t.Errorf("payload landed at %q, which is not a plain filename", got)
			}
			// And the payload still carries the real name: this is a filename
			// rule, not a redaction of what the backend receives.
			body, err := os.ReadFile(filepath.Join(dir, got)) // #nosec G304 -- the path is the temp dir this test created
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Workload string `json:"workload"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Workload != name {
				t.Errorf("payload workload = %q, want the unmodified %q", payload.Workload, name)
			}
		})
	}
}

// TestWriteRefusesANameThatIsNotAPlainFilename guards the choke point rather
// than the callers. Every payload this package writes goes through write(), so
// a future caller that forgets fileToken is stopped here.
func TestWriteRefusesANameThatIsNotAPlainFilename(t *testing.T) {
	s, dir := newTestSpool(t)
	for _, name := range []string{"../escape.json", "sub/dir.json", `..\escape.json`, ".", ".."} {
		if err := s.write(name, struct{}{}); err == nil {
			t.Errorf("write(%q) was allowed", name)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("refused writes still left %d entries behind", len(entries))
	}
}

// TestTheSpoolIsBoundedByBytesOldestFirst: a crash-looping pod produces one OOM
// payload per restart, and nothing acknowledges payloads yet, so without a size
// bound the spool grows until the node's ephemeral storage is gone and the
// kubelet begins evicting other workloads.
func TestTheSpoolIsBoundedByBytesOldestFirst(t *testing.T) {
	s, dir := newTestSpool(t)
	s.maxBytes = 400 // a handful of the files written below
	s.maxFiles = 1000

	base := time.Now().Add(-time.Hour)
	for i := range 20 {
		name := fmt.Sprintf("payload-%02d.json", i)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		// Ascending mtimes, so "oldest" is unambiguous.
		stamp := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Sweep(time.Now()); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	var names []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
		names = append(names, e.Name())
	}
	if total > s.maxBytes {
		t.Errorf("spool holds %d bytes after the sweep, over the %d budget", total, s.maxBytes)
	}
	if len(names) == 0 {
		t.Fatal("the sweep emptied the spool; it must drop the oldest, not everything")
	}
	sort.Strings(names)
	// The survivors are the newest, so the lowest-numbered file must be gone.
	if names[0] == "payload-00.json" {
		t.Errorf("the oldest payload survived while newer ones were dropped: %v", names)
	}
}

// TestTheSpoolIsBoundedByFileCount: bytes alone do not bound the count. The
// smallest payload is a few hundred bytes, so a byte budget permits millions of
// files — enough to exhaust inodes and to make every sweep's listing expensive.
func TestTheSpoolIsBoundedByFileCount(t *testing.T) {
	s, dir := newTestSpool(t)
	s.maxBytes = 1 << 30 // effectively unbounded
	s.maxFiles = 5

	base := time.Now().Add(-time.Hour)
	for i := range 20 {
		path := filepath.Join(dir, fmt.Sprintf("payload-%02d.json", i))
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Sweep(time.Now()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > s.maxFiles {
		t.Errorf("spool holds %d files after the sweep, over the %d budget", len(entries), s.maxFiles)
	}
}

// TestTheSpoolIsBoundedOnAClusterThatWritesNoUsage is the reachability half of
// the fix, stated as the cluster it was broken on.
//
// The sweep used to run only from WriteUsageSnapshot, which runs only when there
// are usage records. On a cluster where the kubelet cannot be polled — the
// nodes/proxy grant withheld, or every kubelet unreachable — there are none, so
// the 24h age cap was never applied and the spool grew without bound while the
// agent looked healthy.
func TestTheSpoolIsBoundedOnAClusterThatWritesNoUsage(t *testing.T) {
	s, dir := newTestSpool(t)

	// A day of OOM kills from a crash-looping pod, and not one usage record.
	for i := range 50 {
		e := collector.OOMKill{
			Namespace: "acme", Pod: "worker-0", Container: "app",
			RestartCount: int32(i),
			FinishedAt:   time.Now().Add(-48 * time.Hour),
		}
		if err := s.WriteOOMKill(e); err != nil {
			t.Fatal(err)
		}
	}
	// Age them past the cap the way an outage would.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-48 * time.Hour)
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(dir, e.Name()), stale, stale); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Sweep(time.Now()); err != nil {
		t.Fatal(err)
	}

	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%d expired payloads survived on a cluster with no usage records", len(entries))
	}
}
