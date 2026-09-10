package sink

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/journal"
	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

var recoverWindow = time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

// midWindow is an instant inside recoverWindow, where a restart lands.
var midWindow = recoverWindow.Add(30 * time.Minute)

func recoverSpool(t *testing.T) (*Spool, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// openWindowRecord is one record of the window under test, carrying enough
// distinct numbers that a lossy round trip shows up.
func openWindowRecord() *rollup.Record {
	acc := rollup.NewAccumulator(time.Hour)
	key := rollup.Key{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"}
	acc.ObserveCPUDelta(key, recoverWindow, recoverWindow.Add(10*time.Minute), 60e9)
	acc.ObserveCPUDelta(key, recoverWindow.Add(10*time.Minute), recoverWindow.Add(20*time.Minute), 120e9)
	acc.ObserveThrottling(key, recoverWindow, recoverWindow.Add(20*time.Minute), 7, 900)
	acc.ObserveMemory(key, recoverWindow.Add(5*time.Minute), 64<<20)
	acc.ObserveMemory(key, recoverWindow.Add(15*time.Minute), 96<<20)
	return acc.Snapshots()[0]
}

func writeOpenWindow(t *testing.T, s *Spool) *rollup.Record {
	t.Helper()
	r := openWindowRecord()
	if err := s.WriteUsageSnapshot([]*rollup.Record{r}, model.Observation{}); err != nil {
		t.Fatal(err)
	}
	return r
}

func snapshotName(start time.Time) string {
	return windowKey{start: start, seconds: 3600}.name() + usageSnapshotSuffix
}

// The whole point: what one process snapshotted, the next one reads back
// unchanged, down to the histogram buckets.
func TestAnOpenWindowIsRecoveredExactly(t *testing.T) {
	s, _ := recoverSpool(t)
	want := writeOpenWindow(t, s)

	rec, err := s.RecoverOpenWindows(midWindow)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Windows != 1 || len(rec.Records) != 1 || len(rec.Skipped) != 0 {
		t.Fatalf("recovered %d windows / %d records / %d skipped, want 1/1/0",
			rec.Windows, len(rec.Records), len(rec.Skipped))
	}
	got := rec.Records[0]
	if got.Key != want.Key || !got.WindowStart.Equal(want.WindowStart) || got.WindowSeconds != want.WindowSeconds {
		t.Fatalf("identity = %+v %s/%d, want %+v %s/%d",
			got.Key, got.WindowStart, got.WindowSeconds, want.Key, want.WindowStart, want.WindowSeconds)
	}
	if got.CPU.CoreNanoseconds != want.CPU.CoreNanoseconds ||
		got.CPU.CoveredNanoseconds != want.CPU.CoveredNanoseconds ||
		got.CPU.ThrottledPeriods != want.CPU.ThrottledPeriods ||
		got.CPU.Samples != want.CPU.Samples || got.Memory.SumBytes != want.Memory.SumBytes {
		t.Errorf("totals = %+v / %+v, want %+v / %+v", got.CPU, got.Memory, want.CPU, want.Memory)
	}
	if !maps2Equal(got.CPU.Hist.Counts(), want.CPU.Hist.Counts()) {
		t.Errorf("cpu buckets = %v, want %v", got.CPU.Hist.Counts(), want.CPU.Hist.Counts())
	}
	if !maps2Equal(got.Memory.Hist.Counts(), want.Memory.Hist.Counts()) {
		t.Errorf("memory buckets = %v, want %v", got.Memory.Hist.Counts(), want.Memory.Hist.Counts())
	}

	if c := s.Counters().Recovered; c.Windows != 1 || c.Records != 1 || c.Skipped != 0 {
		t.Errorf("recovery counters = %+v, want 1 window, 1 record, 0 skipped", c)
	}
}

func maps2Equal(a, b map[int]uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A spool that holds nothing must give exactly the behaviour of an
// installation that has never had one: the invariant that lost state is
// harmless is what makes this recovery safe to add at all.
func TestAnEmptySpoolRecoversNothingAndSaysSo(t *testing.T) {
	s, _ := recoverSpool(t)
	rec, err := s.RecoverOpenWindows(midWindow)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Windows != 0 || len(rec.Records) != 0 || len(rec.Skipped) != 0 {
		t.Fatalf("recovered %+v from an empty spool, want nothing", rec)
	}
}

// A window whose end has passed is not reopened. Its snapshot ships as it is
// and expires with the sweep; adopting it would put an accumulator back into an
// hour the cluster has left.
func TestAWindowThatHasEndedIsNotReopened(t *testing.T) {
	s, _ := recoverSpool(t)
	writeOpenWindow(t, s)

	rec, err := s.RecoverOpenWindows(recoverWindow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Windows != 0 {
		t.Fatalf("recovered %d windows past the window's end, want 0", rec.Windows)
	}
}

// A window that already has a closed record was made final. Reopening it would
// let the next close supersede that record with a subset of itself.
func TestAWindowWithAClosedRecordIsNotReopened(t *testing.T) {
	s, dir := recoverSpool(t)
	writeOpenWindow(t, s)
	closed := filepath.Join(dir, windowKey{start: recoverWindow, seconds: 3600}.name()+".json")
	if err := os.WriteFile(closed, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec, err := s.RecoverOpenWindows(midWindow)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Windows != 0 || len(rec.Skipped) != 0 {
		t.Fatalf("recovered %+v beside a closed record, want nothing and no complaint", rec)
	}
}

// A file this process cannot use is skipped and reported — and left exactly
// where it is. It is still a payload the backend can ingest, and deleting it on
// the strength of a failed read would destroy deliverable data.
func TestAnUnusableFileIsSkippedAndNeverRemoved(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{name: "truncated by a crash", content: `{"kind":"usage_snapshot","reco`, want: "decoding"},
		{name: "another kind entirely", content: `{"kind":"network_window"}`, want: `kind "network_window"`},
		{
			name:    "a window other than its name",
			content: `{"kind":"usage_snapshot","window_start":"2026-08-06T09:00:00Z","window_seconds":3600}`,
			want:    "it holds the window starting",
		},
		{
			name: "a record from another window",
			content: `{"kind":"usage_snapshot","window_start":"2026-08-06T10:00:00Z","window_seconds":3600,` +
				`"records":[{"window_start":"2026-08-06T09:00:00Z","window_seconds":3600}]}`,
			want: "belongs to another window",
		},
		{
			name: "a histogram off the grid",
			content: `{"kind":"usage_snapshot","window_start":"2026-08-06T10:00:00Z","window_seconds":3600,` +
				`"records":[{"window_start":"2026-08-06T10:00:00Z","window_seconds":3600,` +
				`"cpu":{"hist_milli":{"15":3}}}]}`,
			want: "not a bucket bound",
		},
	}
	for _, c := range cases {
		s, dir := recoverSpool(t)
		path := filepath.Join(dir, snapshotName(recoverWindow))
		if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
			t.Fatal(err)
		}
		rec, err := s.RecoverOpenWindows(midWindow)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(rec.Records) != 0 {
			t.Errorf("%s: recovered %d records, want none", c.name, len(rec.Records))
		}
		if len(rec.Skipped) != 1 {
			t.Fatalf("%s: %d files reported skipped, want 1", c.name, len(rec.Skipped))
		}
		if got := rec.Skipped[0].Error(); !strings.Contains(got, c.want) {
			t.Errorf("%s: skipped because %q, want it to mention %q", c.name, got, c.want)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: the file is gone: %v — an unreadable payload is still a shippable one", c.name, err)
		}
	}
}

// The ceiling is on the file, not on the spool: a single payload past it is
// corrupt rather than large, and decoding it would be the one unbounded
// allocation in the agent.
func TestAFilePastTheCeilingIsNotDecoded(t *testing.T) {
	s, dir := recoverSpool(t)
	path := filepath.Join(dir, snapshotName(recoverWindow))
	if err := os.WriteFile(path, make([]byte, maxRecoveryBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := s.RecoverOpenWindows(midWindow)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Skipped) != 1 || !strings.Contains(rec.Skipped[0].Error(), "ceiling") {
		t.Fatalf("skipped = %v, want one refusal naming the ceiling", rec.Skipped)
	}
}

// Every other kind in the spool is invisible to this read. It looks for its own
// snapshots by name and opens nothing else.
func TestNoPayloadOutsideTheResumingKindsIsOpened(t *testing.T) {
	s, dir := recoverSpool(t)
	writeOpenWindow(t, s)
	for _, name := range []string{
		"workload-metadata.json",
		"collection-coverage.json",
		"restart-counters.json",
		windowKey{start: recoverWindow, seconds: 3600}.name() + ".json.tmp",
		"usage-not-a-number-3600.snapshot.json",
		"usage-" + strconv.FormatInt(recoverWindow.Unix(), 10) + "-0.snapshot.json",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not json at all"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	rec, err := s.RecoverOpenWindows(midWindow)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Windows != 1 || rec.JournalWindows != 0 || len(rec.Skipped) != 0 {
		t.Fatalf("recovered %d usage and %d journal windows with %d complaints, want 1, 0 and none: "+
			"only an open window of a kind that resumes is read", rec.Windows, rec.JournalWindows, len(rec.Skipped))
	}
}

// restartRecords is one window's worth of restarts, distinct enough that losing
// any of them shows up in a count.
func restartRecords(pods ...string) []journal.RestartRecord {
	out := make([]journal.RestartRecord, 0, len(pods))
	for i, pod := range pods {
		out = append(out, journal.RestartRecord{
			Key:           journal.Key{Namespace: "shop", Pod: pod, Container: "app"},
			Workload:      model.WorkloadRef{Kind: "Deployment", Name: "web"},
			WindowStart:   recoverWindow,
			WindowSeconds: 3600,
			Restarts:      int64(i + 1),
			Reasons:       map[string]int64{"OOMKilled": int64(i + 1)},
		})
	}
	return out
}

func TestAnOpenJournalWindowIsRecovered(t *testing.T) {
	s, _ := recoverSpool(t)
	if err := s.WriteContainerRestarts(restartRecords("web-a", "web-b")); err != nil {
		t.Fatal(err)
	}

	rec, err := s.RecoverOpenWindows(midWindow)
	if err != nil {
		t.Fatal(err)
	}
	if rec.JournalWindows != 1 || len(rec.Restarts) != 2 {
		t.Fatalf("recovered %d journal windows carrying %d records, want 1 and 2",
			rec.JournalWindows, len(rec.Restarts))
	}
	if rec.Restarts[0].Reasons["OOMKilled"] == 0 || rec.Restarts[1].Restarts != 2 {
		t.Errorf("a recovered record lost its contents: %+v", rec.Restarts)
	}
}

// A window that has ended is already its own final record. Reopening it would
// let this process write a subset of it back over itself, which is the failure
// this whole read exists to prevent (ADR 0077).
func TestAJournalWindowThatHasEndedIsNotReopened(t *testing.T) {
	s, _ := recoverSpool(t)
	if err := s.WriteContainerRestarts(restartRecords("web-a")); err != nil {
		t.Fatal(err)
	}

	rec, err := s.RecoverOpenWindows(recoverWindow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rec.JournalWindows != 0 || len(rec.Restarts) != 0 {
		t.Fatalf("an ended window was reopened: %d windows, %d records",
			rec.JournalWindows, len(rec.Restarts))
	}
}

// The failure this commit exists for, end to end.
//
// A restart empties the accumulator, and the collector rebaselines on first
// sight without re-emitting what it already counted. So a container restarting
// later in the same window used to make the flush write a window holding only
// that one — over a file that held every restart before it, under the same
// natural key, which the contract tells the backend is the complete state.
func TestARestartInsideAnOpenWindowKeepsWhatTheWindowAlreadyHeld(t *testing.T) {
	s, dir := recoverSpool(t)
	if err := s.WriteContainerRestarts(restartRecords("web-a", "web-b")); err != nil {
		t.Fatal(err)
	}

	// The process restarts: a fresh accumulator, seeded from the spool.
	resumed := journal.NewRestarts(time.Hour)
	rec, err := s.RecoverOpenWindows(midWindow)
	if err != nil {
		t.Fatal(err)
	}
	if seeded := resumed.Resume(rec.Restarts); seeded != 2 {
		t.Fatalf("seeded %d records into the accumulator, want 2", seeded)
	}

	// A third container restarts inside the same still-open window, and the
	// flush writes what the accumulator holds, exactly as cmd/agent does.
	resumed.Observe(model.ContainerRestart{
		Namespace: "shop", Pod: "web-c", Container: "app",
		Workload:   model.WorkloadRef{Kind: "Deployment", Name: "web"},
		ObservedAt: midWindow, Restarts: 1, Reason: "Error",
	})
	records := append(resumed.CloseBefore(midWindow), resumed.Snapshots()...)
	if err := s.WriteContainerRestarts(records); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, journalWindowName(restartsNamePrefix, windowKey{start: recoverWindow, seconds: 3600}))) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	var payload journalWindow[journal.RestartRecord]
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	pods := map[string]bool{}
	for _, r := range payload.Records {
		pods[r.Pod] = true
	}
	for _, want := range []string{"web-a", "web-b", "web-c"} {
		if !pods[want] {
			t.Errorf("the window written after the restart lost %s; it holds %v", want, pods)
		}
	}
}
