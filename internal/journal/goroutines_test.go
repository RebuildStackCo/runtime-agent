package journal

import (
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

var hourStart = time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)

func reading(at time.Time, n int64) model.GoroutineCount {
	return model.GoroutineCount{
		Namespace:   "shop",
		Pod:         "web-0",
		Container:   "app",
		Workload:    model.WorkloadRef{Kind: "Deployment", Name: "web"},
		ImageDigest: "sha256:a",
		ObservedAt:  at,
		Goroutines:  n,
	}
}

func TestReadingsLandInTheWindowHoldingTheirInstant(t *testing.T) {
	g := NewGoroutines(time.Hour)
	g.Observe(reading(hourStart.Add(time.Minute), 100))
	g.Observe(reading(hourStart.Add(2*time.Minute), 140))
	g.Observe(reading(hourStart.Add(time.Hour+time.Minute), 190))

	got := g.Snapshots()
	if len(got) != 2 {
		t.Fatalf("got %d records, want one per window: %+v", len(got), got)
	}
	if !got[0].WindowStart.Equal(hourStart) || len(got[0].Samples) != 2 {
		t.Errorf("first window = %s with %d samples, want %s with 2",
			got[0].WindowStart, len(got[0].Samples), hourStart)
	}
	if !got[1].WindowStart.Equal(hourStart.Add(time.Hour)) || len(got[1].Samples) != 1 {
		t.Errorf("second window = %s with %d samples, want the next hour with 1",
			got[1].WindowStart, len(got[1].Samples))
	}
	if got[0].Samples[0].Goroutines != 100 || got[0].Samples[1].Goroutines != 140 {
		t.Errorf("samples = %+v, want the readings in the order taken", got[0].Samples)
	}
}

// The series is the whole point, so a record is a series and not a last value:
// two readings of one process in one window are two samples, never one
// overwriting the other.
func TestASecondReadingOfOneProcessExtendsTheSeries(t *testing.T) {
	g := NewGoroutines(time.Hour)
	for i := range 5 {
		g.Observe(reading(hourStart.Add(time.Duration(i)*time.Minute), int64(100+i)))
	}
	got := g.Snapshots()
	if len(got) != 1 || len(got[0].Samples) != 5 {
		t.Fatalf("got %d records with %d samples, want one record with five", len(got), len(got[0].Samples))
	}
}

func TestCloseBeforeReturnsEndedWindowsAndKeepsOpenOnes(t *testing.T) {
	g := NewGoroutines(time.Hour)
	g.Observe(reading(hourStart.Add(time.Minute), 100))
	g.Observe(reading(hourStart.Add(time.Hour+time.Minute), 190))

	closed := g.CloseBefore(hourStart.Add(time.Hour + 30*time.Minute))
	if len(closed) != 1 || !closed[0].WindowStart.Equal(hourStart) {
		t.Fatalf("closed = %+v, want only the first hour", closed)
	}
	// Removal is what bounds memory: a closed window must be gone, not merely
	// reported.
	remaining := g.Snapshots()
	if len(remaining) != 1 || !remaining[0].WindowStart.Equal(hourStart.Add(time.Hour)) {
		t.Errorf("remaining = %+v, want only the open hour", remaining)
	}
}

func TestResumeSeedsAnAbsentWindowAndLeavesAnObservedOneAlone(t *testing.T) {
	g := NewGoroutines(time.Hour)
	g.Observe(reading(hourStart.Add(30*time.Minute), 300))

	spooled := GoroutineRecord{
		Key:           Key{Namespace: "shop", Pod: "web-0", Container: "app"},
		Workload:      model.WorkloadRef{Kind: "Deployment", Name: "web"},
		WindowStart:   hourStart,
		WindowSeconds: 3600,
		Samples:       []GoroutineSample{{At: hourStart, Goroutines: 100}},
	}
	other := spooled
	other.Key = Key{Namespace: "search", Pod: "index-0", Container: "app"}

	if got := g.Resume([]GoroutineRecord{spooled, other}); got != 1 {
		t.Errorf("seeded %d records, want only the key this process had not seen", got)
	}
	for _, rec := range g.Snapshots() {
		if rec.Pod == "web-0" && len(rec.Samples) != 1 {
			t.Errorf("the observed key holds %d samples, want this process's own reading alone",
				len(rec.Samples))
		}
	}
}

// A resumed record is bounded on the way in too: the cap is a property of the
// accumulator, and a file holding more than it — a payload from another build,
// or one this process must not trust — may not raise it.
func TestResumeBoundsWhatItAdopts(t *testing.T) {
	samples := make([]GoroutineSample, maxSamplesPerWindow+5)
	for i := range samples {
		samples[i] = GoroutineSample{At: hourStart.Add(time.Duration(i) * time.Second), Goroutines: 1}
	}
	g := NewGoroutines(time.Hour)
	g.Resume([]GoroutineRecord{{
		Key:           Key{Namespace: "shop", Pod: "web-0", Container: "app"},
		WindowStart:   hourStart,
		WindowSeconds: 3600,
		Samples:       samples,
	}})
	got := g.Snapshots()
	if len(got) != 1 {
		t.Fatalf("got %d records, want one", len(got))
	}
	if len(got[0].Samples) != maxSamplesPerWindow || got[0].SamplesDropped != 5 {
		t.Errorf("kept %d samples and dropped %d, want %d kept and 5 counted as dropped",
			len(got[0].Samples), got[0].SamplesDropped, maxSamplesPerWindow)
	}
}

// Past the cap the series thins, and the payload says so. A reader has to be
// able to tell a gap in the series from a gap in the process.
func TestSamplesPastTheCapAreDroppedAndCounted(t *testing.T) {
	g := NewGoroutines(time.Hour)
	for i := range maxSamplesPerWindow + 3 {
		g.Observe(reading(hourStart.Add(time.Duration(i)*time.Second), 100))
	}
	got := g.Snapshots()
	if len(got[0].Samples) != maxSamplesPerWindow {
		t.Errorf("kept %d samples, want the cap of %d", len(got[0].Samples), maxSamplesPerWindow)
	}
	if got[0].SamplesDropped != 3 {
		t.Errorf("dropped %d, want 3 counted", got[0].SamplesDropped)
	}
}

// A live Go process runs at least one goroutine, so a count of zero is a
// misread rather than a quiet workload, and it must not enter the series.
func TestANonPositiveCountIsNotASample(t *testing.T) {
	g := NewGoroutines(time.Hour)
	g.Observe(reading(hourStart, 0))
	if got := g.Snapshots(); len(got) != 0 {
		t.Errorf("got %+v, want no record at all", got)
	}
}

// Callers retain snapshots, so a snapshot must not be a window into the
// accumulator's own memory.
func TestASnapshotIsACopy(t *testing.T) {
	g := NewGoroutines(time.Hour)
	g.Observe(reading(hourStart, 100))

	got := g.Snapshots()
	got[0].Samples[0].Goroutines = 999

	if again := g.Snapshots(); again[0].Samples[0].Goroutines != 100 {
		t.Errorf("the accumulator now holds %d; a snapshot shares its memory",
			again[0].Samples[0].Goroutines)
	}
}
