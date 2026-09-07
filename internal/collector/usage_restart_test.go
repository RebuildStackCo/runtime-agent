package collector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/utils/ptr"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

// The restart scenario, shared by both processes in it. The container starts
// two hours before the window under test and burns nothing until that window
// opens, so a counter smeared over the container's whole lifetime is
// arithmetically distinguishable from the deltas observed inside the window.
var (
	restartContainerStart = time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC)
	restartWindowStart    = time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
)

// restartCounter is the container's cumulative CPU counter: zero until the
// window opens, then a steady 100 millicores.
func restartCounter(at time.Time) uint64 {
	if !at.After(restartWindowStart) {
		return 0
	}
	return uint64(at.Sub(restartWindowStart).Seconds()) * 1e8
}

func restartSummary(at time.Time) *statsapi.Summary {
	return summaryWith(statsapi.ContainerStats{
		Name:      "app",
		StartTime: metav1.NewTime(restartContainerStart),
		CPU: &statsapi.CPUStats{
			Time:                 metav1.NewTime(at),
			UsageCoreNanoSeconds: ptr.To(restartCounter(at)),
		},
	})
}

// restartProcess is one controller lifetime: a poller wired to the spool the
// way cmd/agent wires it, polled at every instant given, then flushed once.
func restartProcess(t *testing.T, spool *sink.Spool, at ...time.Time) {
	t.Helper()
	p := NewUsagePoller(nil, func() []string { return nil }, webResolver(),
		func(records []*rollup.Record) {
			if err := spool.WriteUsageSnapshot(records, model.Observation{}); err != nil {
				t.Errorf("writing usage snapshot: %v", err)
			}
		},
		func(records []*rollup.Record) {
			if err := spool.WriteClosedWindows(records, model.Observation{}); err != nil {
				t.Errorf("writing closed windows: %v", err)
			}
		},
		nil, nil)
	// What cmd/agent does at startup, in the same order: read the spool, then
	// seed, then poll.
	rec, err := spool.RecoverOpenWindows(at[0])
	if err != nil {
		t.Fatalf("recovering open windows: %v", err)
	}
	for _, skipped := range rec.Skipped {
		t.Errorf("a spool file was skipped: %v", skipped)
	}
	if err := p.Seed(rec.Records); err != nil {
		t.Fatalf("seeding the accumulator: %v", err)
	}
	for _, now := range at {
		p.ingest(restartSummary(now), now)
	}
	p.flush(at[len(at)-1])
}

// spooledRecords reads one payload file back into the records it holds.
func spooledRecords(t *testing.T, dir, name string) []*rollup.Record {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- test-controlled dir
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	var payload struct {
		Records []*rollup.Record `json:"records"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decoding %s: %v", name, err)
	}
	return payload.Records
}

func windowFile(start time.Time, suffix string) string {
	return fmt.Sprintf("usage-%d-3600%s.json", start.Unix(), suffix)
}

// A controller restart lands mid-window. The window is UTC-aligned and the
// accumulator is memory-only, so the second process opens the same window from
// nothing — and its first snapshot is written over the file holding the first
// process's accumulation.
func TestARestartKeepsTheOpenWindowTheSpoolAlreadyHolds(t *testing.T) {
	dir := t.TempDir()
	spool, err := sink.NewSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}

	// First process: baselines at the window boundary, then three ten-minute
	// deltas of 100 millicores — 180 core-seconds over 30 observed minutes.
	restartProcess(t, spool,
		restartWindowStart,
		restartWindowStart.Add(10*time.Minute),
		restartWindowStart.Add(20*time.Minute),
		restartWindowStart.Add(30*time.Minute))

	// Second process: it re-baselines at 10:40 and takes one more delta.
	restartProcess(t, spool,
		restartWindowStart.Add(40*time.Minute),
		restartWindowStart.Add(50*time.Minute))

	records := spooledRecords(t, dir, windowFile(restartWindowStart, ".snapshot"))
	if len(records) != 1 {
		t.Fatalf("got %d records in the open window, want 1", len(records))
	}
	r := records[0]
	if want := int64(240 * time.Second); r.CPU.CoreNanoseconds != want {
		t.Errorf("core nanoseconds = %d, want %d: the snapshot must hold the 30 minutes the "+
			"previous process accumulated as well as the 10 this one did",
			r.CPU.CoreNanoseconds, want)
	}
	if want := int64(40 * time.Minute); r.CPU.CoveredNanoseconds != want {
		t.Errorf("covered nanoseconds = %d, want %d", r.CPU.CoveredNanoseconds, want)
	}
	if r.CPU.Samples != 4 {
		t.Errorf("cpu samples = %d, want 4", r.CPU.Samples)
	}
}

// The same restart reaches further back than the open window: the first
// observation of the new process attributes the counter from container start,
// so it reopens windows that were closed and shipped, and their final records
// are rewritten with a fragment of themselves.
func TestARestartDoesNotRewriteAWindowItAlreadyClosed(t *testing.T) {
	dir := t.TempDir()
	spool, err := sink.NewSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}

	restartProcess(t, spool,
		restartWindowStart,
		restartWindowStart.Add(30*time.Minute))

	before := spooledRecords(t, dir, windowFile(restartContainerStart.Add(time.Hour), ""))

	restartProcess(t, spool,
		restartWindowStart.Add(40*time.Minute),
		restartWindowStart.Add(50*time.Minute))

	after := spooledRecords(t, dir, windowFile(restartContainerStart.Add(time.Hour), ""))
	if len(before) != len(after) || before[0].CPU.CoreNanoseconds != after[0].CPU.CoreNanoseconds {
		t.Errorf("the closed 09:00 window was rewritten across a restart: %d core ns, was %d — "+
			"CPU burned after 10:00 attributed to an hour that was already final",
			after[0].CPU.CoreNanoseconds, before[0].CPU.CoreNanoseconds)
	}
}

// seededPoller is a poller that resumed one key's open window, as cmd/agent
// leaves it after the startup read.
func seededPoller(t *testing.T, container string) *UsagePoller {
	t.Helper()
	acc := rollup.NewAccumulator(UsageWindowLength)
	acc.ObserveCPUDelta(rollup.Key{Namespace: "shop", WorkloadKind: "Deployment",
		WorkloadName: "web", Container: container},
		restartWindowStart, restartWindowStart.Add(30*time.Minute), 180e9)

	p := testPoller(webResolver())
	if err := p.Seed(acc.Snapshots()); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return p
}

// The suppression is per key, not per process. A container the resumed window
// says nothing about is a container this agent has not observed, and its
// counter still counts from container start — which is what fills the gap when
// a pod is rescheduled onto a node with an empty spool.
func TestAKeyTheSpoolDidNotHoldIsStillAttributedFromContainerStart(t *testing.T) {
	p := seededPoller(t, "app")

	at := restartWindowStart.Add(40 * time.Minute)
	summary := restartSummary(at)
	summary.Pods[0].Containers = append(summary.Pods[0].Containers, statsapi.ContainerStats{
		Name:      "sidecar",
		StartTime: metav1.NewTime(restartWindowStart.Add(35 * time.Minute)),
		CPU: &statsapi.CPUStats{
			Time:                 metav1.NewTime(at),
			UsageCoreNanoSeconds: ptr.To(uint64(30e9)),
		},
	})
	p.ingest(summary, at)

	for _, r := range p.acc.Snapshots() {
		if r.Container != "sidecar" {
			continue
		}
		if r.CPU.CoreNanoseconds != 30e9 {
			t.Errorf("sidecar core nanos = %d, want 30e9: an unseen container is attributed "+
				"from its start whatever the seed held", r.CPU.CoreNanoseconds)
		}
		return
	}
	t.Fatal("no record for the sidecar")
}

// The guard is only worth what it protects. Once the resumed window has ended
// there is nothing left to attribute twice into, and the rule that fills a gap
// from container start comes back.
func TestTheResumeGuardLiftsOnceItsWindowHasEnded(t *testing.T) {
	p := seededPoller(t, "app")
	after := restartWindowStart.Add(90 * time.Minute)
	p.sweep(after)

	key := rollup.Key{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"}
	if !p.attributableFromStart(key) {
		t.Error("the resumed key is still guarded an hour after its window closed")
	}
}

// Nothing to seed must leave the poller exactly where an installation with no
// spool at all is: the loss-harmless invariant is what lets this recovery exist.
func TestSeedingNothingChangesNothing(t *testing.T) {
	at := restartWindowStart.Add(10 * time.Minute)

	seeded := testPoller(webResolver())
	if err := seeded.Seed(nil); err != nil {
		t.Fatalf("seeding nothing: %v", err)
	}
	seeded.ingest(restartSummary(at), at)

	untouched := testPoller(webResolver())
	untouched.ingest(restartSummary(at), at)

	got, _ := json.Marshal(seeded.acc.Snapshots())
	want, _ := json.Marshal(untouched.acc.Snapshots())
	if string(got) != string(want) {
		t.Errorf("an empty seed changed what the poller accumulated\n got %s\nwant %s", got, want)
	}
}
