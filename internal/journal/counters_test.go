package journal

import (
	"testing"
	"time"
)

var counterBase = stateT0.Add(-time.Hour)

// reading is the one counter these tests move.
func counterReading(n int64) map[string]int64 {
	return map[string]int64{"filter.excluded_pod_annotation": n}
}

// oneCounter returns the single record, failing if the accumulator holds any
// other number of them.
func oneCounter(t *testing.T, c *Counters) CounterRecord {
	t.Helper()
	got := c.Snapshots()
	if len(got) != 1 {
		t.Fatalf("holding %d records, want exactly one: %+v", len(got), got)
	}
	return got[0]
}

// The first pass has nothing to subtract from. Treating the total as the rise
// would put a whole process's history into whatever window it restarted in.
func TestTheFirstPassOfCountersAccruesNothing(t *testing.T) {
	c := NewCounters(time.Hour)
	c.Observe(stateT0, counterBase, counterReading(412))

	rec := oneCounter(t, c)
	if rec.Delta != 0 || rec.ObservedNanos != 0 {
		t.Errorf("first pass accrued delta=%d observed=%d, want both zero", rec.Delta, rec.ObservedNanos)
	}
}

// A counter that did not move is not a counter nobody watched: the record
// exists, carrying the observation that says so.
func TestACounterThatDidNotMoveStillCarriesItsObservation(t *testing.T) {
	c := NewCounters(time.Hour)
	c.Observe(stateT0, counterBase, counterReading(412))
	c.Observe(stateT0.Add(2*time.Minute), counterBase, counterReading(412))

	rec := oneCounter(t, c)
	if rec.Delta != 0 {
		t.Errorf("delta %d, want 0", rec.Delta)
	}
	if want := (2 * time.Minute).Nanoseconds(); rec.ObservedNanos != want {
		t.Errorf("observed %d, want %d", rec.ObservedNanos, want)
	}
}

func TestARiseLandsInTheWindowThePassSawIt(t *testing.T) {
	c := NewCounters(time.Hour)
	c.Observe(stateT0, counterBase, counterReading(412))
	c.Observe(stateT0.Add(time.Minute), counterBase, counterReading(424))

	if rec := oneCounter(t, c); rec.Delta != 12 {
		t.Errorf("delta %d, want 12", rec.Delta)
	}
}

// A moved base is a restarted process, and its readings are not subtractable
// from this one's. The rule ADR 0062 set for PIDs, keyed by `since` (ADR 0089).
func TestAMovedBaseProducesNoDelta(t *testing.T) {
	c := NewCounters(time.Hour)
	c.Observe(stateT0, counterBase, counterReading(412))
	c.Observe(stateT0.Add(time.Minute), stateT0, counterReading(3))

	rec := oneCounter(t, c)
	if rec.Delta != 0 {
		t.Errorf("delta %d across a moved base, want 0", rec.Delta)
	}
	// And the pass after it subtracts from the new base normally.
	c.Observe(stateT0.Add(2*time.Minute), stateT0, counterReading(9))
	if rec := oneCounter(t, c); rec.Delta != 6 {
		t.Errorf("delta %d after the base settled, want 6", rec.Delta)
	}
}

// A counter that fell under an unchanged base cannot be read as a negative
// rise: whatever produced it, the reading is not of the series the last one
// described.
func TestACounterThatFellProducesNoDelta(t *testing.T) {
	c := NewCounters(time.Hour)
	c.Observe(stateT0, counterBase, counterReading(412))
	c.Observe(stateT0.Add(time.Minute), counterBase, counterReading(7))

	if rec := oneCounter(t, c); rec.Delta != 0 {
		t.Errorf("delta %d for a counter that fell, want 0", rec.Delta)
	}
}

// The interval is a duration and splits at the boundary; the rise is not, and
// lands whole where it was seen. Splitting it would be inventing an instant.
func TestAnIntervalSplitsAtTheBoundaryAndTheRiseDoesNot(t *testing.T) {
	c := NewCounters(time.Hour)
	c.Observe(stateT0.Add(50*time.Minute), counterBase, counterReading(100))
	c.Observe(stateT0.Add(70*time.Minute), counterBase, counterReading(130))

	got := c.Snapshots()
	if len(got) != 2 {
		t.Fatalf("holding %d records, want one per window: %+v", len(got), got)
	}
	first, second := got[0], got[1]
	if want := (10 * time.Minute).Nanoseconds(); first.ObservedNanos != want {
		t.Errorf("first window observed %d, want %d", first.ObservedNanos, want)
	}
	if want := (10 * time.Minute).Nanoseconds(); second.ObservedNanos != want {
		t.Errorf("second window observed %d, want %d", second.ObservedNanos, want)
	}
	if first.Delta != 0 || second.Delta != 30 {
		t.Errorf("deltas %d and %d, want 0 and 30 — the rise lands where it was seen",
			first.Delta, second.Delta)
	}
}

// A restart inside an open window keeps what the window already held and adds
// only what this process counted, exactly as the state records do.
func TestAResumedCounterWindowKeepsWhatItHeld(t *testing.T) {
	first := NewCounters(time.Hour)
	first.Observe(stateT0, counterBase, counterReading(100))
	first.Observe(stateT0.Add(10*time.Minute), counterBase, counterReading(140))
	saved := oneCounter(t, first)
	if saved.Delta != 40 {
		t.Fatalf("the first process accrued %d, want 40", saved.Delta)
	}

	second := NewCounters(time.Hour)
	if seeded := second.Resume([]CounterRecord{saved}); seeded != 1 {
		t.Fatalf("resumed %d records, want 1", seeded)
	}
	// A new process counts from its own base, so its first pass adds nothing.
	second.Observe(stateT0.Add(30*time.Minute), stateT0, counterReading(0))
	second.Observe(stateT0.Add(40*time.Minute), stateT0, counterReading(5))

	rec := oneCounter(t, second)
	if rec.Delta != 45 {
		t.Errorf("delta %d, want 45 — forty before the restart and five after", rec.Delta)
	}
	if want := (20 * time.Minute).Nanoseconds(); rec.ObservedNanos != want {
		t.Errorf("observed %d, want %d — ten minutes each side of the restart, and "+
			"none of the twenty nobody watched", rec.ObservedNanos, want)
	}
}

func TestResumeLeavesACounterKeyThisProcessAlreadyHolds(t *testing.T) {
	c := NewCounters(time.Hour)
	c.Observe(stateT0, counterBase, counterReading(1))
	c.Observe(stateT0.Add(time.Minute), counterBase, counterReading(4))

	stale := CounterRecord{
		Counter:     "filter.excluded_pod_annotation",
		WindowStart: stateT0.Truncate(time.Hour), WindowSeconds: 3600, Delta: 999,
	}
	if seeded := c.Resume([]CounterRecord{stale}); seeded != 0 {
		t.Errorf("resumed %d records over a live one, want 0", seeded)
	}
	if rec := oneCounter(t, c); rec.Delta == 999 {
		t.Error("a record on disk displaced one this process counted")
	}
}

func TestCloseBeforeReleasesEndedCounterWindows(t *testing.T) {
	c := NewCounters(time.Hour)
	c.Observe(stateT0, counterBase, counterReading(1))
	c.Observe(stateT0.Add(70*time.Minute), counterBase, counterReading(2))

	closed := c.CloseBefore(stateT0.Add(80 * time.Minute))
	if len(closed) != 1 {
		t.Fatalf("closed %+v, want the first window alone", closed)
	}
	if got := c.Snapshots(); len(got) != 1 {
		t.Errorf("after closing, holding %+v, want the open window alone", got)
	}
}
