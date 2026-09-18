package journal

import (
	"testing"
	"time"
)

var stateT0 = time.Date(2026, 9, 18, 13, 0, 0, 0, time.UTC)

// failing builds the one state shape these tests use.
func failing(subject string, since *time.Time) []AgentState {
	return []AgentState{{State: StateFailing, Subject: subject, Since: since}}
}

func at(d time.Duration) *time.Time {
	t := stateT0.Add(d)
	return &t
}

// only returns the single record for a subject, failing the test if the
// accumulator holds any other number of them.
func only(t *testing.T, s *States) StateRecord {
	t.Helper()
	got := s.Snapshots()
	if len(got) != 1 {
		t.Fatalf("holding %d records, want exactly one: %+v", len(got), got)
	}
	return got[0]
}

// The first pass has no interval behind it. Accruing from the window's start
// would credit the agent with watching a span it was not running for.
func TestTheFirstPassAccruesNothing(t *testing.T) {
	s := NewStates(time.Hour)
	s.Observe(stateT0.Add(10*time.Minute), failing("services", nil))

	if rec := only(t, s); rec.ObservedNanos != 0 || rec.OccupiedNanos != 0 {
		t.Errorf("first pass accrued observed=%d occupied=%d, want both zero",
			rec.ObservedNanos, rec.OccupiedNanos)
	}
}

// A healthy subject is not an absent one. Observation grows while occupancy
// stays at zero, which is what separates "watched and fine" from "not watched".
func TestAHealthySubjectAccruesObservationAndNoOccupancy(t *testing.T) {
	s := NewStates(time.Hour)
	for i := range 4 {
		s.Observe(stateT0.Add(time.Duration(i)*time.Minute), failing("services", nil))
	}

	rec := only(t, s)
	if want := (3 * time.Minute).Nanoseconds(); rec.ObservedNanos != want {
		t.Errorf("observed %d, want %d", rec.ObservedNanos, want)
	}
	if rec.OccupiedNanos != 0 || rec.Entries != 0 {
		t.Errorf("a healthy subject shows occupancy %d and %d entries", rec.OccupiedNanos, rec.Entries)
	}
}

// The episode that started and ended between two payloads — the case a boolean
// in a superseding payload cannot report at all (ADR 0087, ADR 0088).
func TestAnEpisodeThatEndedIsStillCounted(t *testing.T) {
	s := NewStates(time.Hour)
	s.Observe(stateT0, failing("endpoint_slices", nil))
	s.Observe(stateT0.Add(time.Minute), failing("endpoint_slices", at(30*time.Second)))
	s.Observe(stateT0.Add(2*time.Minute), failing("endpoint_slices", at(30*time.Second)))
	s.Observe(stateT0.Add(3*time.Minute), failing("endpoint_slices", nil))

	rec := only(t, s)
	if rec.Entries != 1 {
		t.Errorf("entries %d, want 1", rec.Entries)
	}
	// Open from 13:00:30 to the last pass that saw it open, at 13:02. The pass
	// that found it closed attributes nothing: when it closed is not knowable
	// from two readings.
	if want := (90 * time.Second).Nanoseconds(); rec.OccupiedNanos != want {
		t.Errorf("occupied %d, want %d", rec.OccupiedNanos, want)
	}
	if rec.OpenSince != nil {
		t.Errorf("a closed episode left open_since at %v", rec.OpenSince)
	}
}

// Two episodes and one long one are different diagnoses, so the pair of numbers
// has to tell them apart.
func TestTwoEpisodesInOneWindowCountTwice(t *testing.T) {
	s := NewStates(time.Hour)
	s.Observe(stateT0, failing("services", nil))
	s.Observe(stateT0.Add(time.Minute), failing("services", at(time.Minute)))
	s.Observe(stateT0.Add(2*time.Minute), failing("services", nil))
	s.Observe(stateT0.Add(10*time.Minute), failing("services", at(10*time.Minute)))

	if rec := only(t, s); rec.Entries != 2 {
		t.Errorf("entries %d, want 2 — two episodes, not one long one", rec.Entries)
	}
}

// An episode running when the window turns over belongs to both windows: its
// occupancy is split at the boundary, and it is one episode, counted once.
func TestAnEpisodeCrossingAWindowIsSplitAndCountedOnce(t *testing.T) {
	s := NewStates(time.Hour)
	since := at(50 * time.Minute)
	s.Observe(stateT0.Add(50*time.Minute), failing("services", since))
	s.Observe(stateT0.Add(70*time.Minute), failing("services", since))

	got := s.Snapshots()
	if len(got) != 2 {
		t.Fatalf("holding %d records, want one per window: %+v", len(got), got)
	}
	first, second := got[0], got[1]
	if want := (10 * time.Minute).Nanoseconds(); first.OccupiedNanos != want {
		t.Errorf("first window occupied %d, want %d", first.OccupiedNanos, want)
	}
	if want := (10 * time.Minute).Nanoseconds(); second.OccupiedNanos != want {
		t.Errorf("second window occupied %d, want %d", second.OccupiedNanos, want)
	}
	if first.Entries != 1 || second.Entries != 0 {
		t.Errorf("entries %d and %d, want 1 and 0: one episode, not two",
			first.Entries, second.Entries)
	}
}

// The invariant a resume turns on: occupancy moves forward from where it was
// left, and is never re-measured from the instant the episode began. Getting
// this wrong counts the whole pre-restart span a second time.
func TestAResumedWindowDoesNotMeasureItsOwnPastAgain(t *testing.T) {
	since := at(0)
	first := NewStates(time.Hour)
	first.Observe(stateT0, failing("services", since))
	first.Observe(stateT0.Add(20*time.Minute), failing("services", since))
	saved := only(t, first)

	// The restart: nothing accrues for the gap, because nothing was watching.
	second := NewStates(time.Hour)
	if seeded := second.Resume([]StateRecord{saved}); seeded != 1 {
		t.Fatalf("resumed %d records, want 1", seeded)
	}
	second.Observe(stateT0.Add(40*time.Minute), failing("services", since))
	second.Observe(stateT0.Add(45*time.Minute), failing("services", since))

	rec := only(t, second)
	if want := (25 * time.Minute).Nanoseconds(); rec.OccupiedNanos != want {
		t.Errorf("occupied %d, want %d — 20 before the restart and 5 after, and "+
			"nothing for the 20 nobody watched", rec.OccupiedNanos, want)
	}
	if rec.Entries != 1 {
		t.Errorf("entries %d, want 1: the resumed episode is the one already counted", rec.Entries)
	}
}

// A resumed process that finds a different episode open counts it, because it
// is one: the marker only silences the episode it already holds.
func TestAResumedProcessCountsANewEpisode(t *testing.T) {
	since := at(0)
	saved := StateRecord{
		State: StateFailing, Subject: "services",
		WindowStart: stateT0, WindowSeconds: 3600,
		AccruedTo: stateT0.Add(20 * time.Minute), OpenSince: since,
	}
	s := NewStates(time.Hour)
	s.Resume([]StateRecord{saved})
	s.Observe(stateT0.Add(40*time.Minute), failing("services", at(35*time.Minute)))

	if rec := only(t, s); rec.Entries != 1 {
		t.Errorf("entries %d, want 1 for the episode that began after the restart", rec.Entries)
	}
}

// Resume never overwrites an observation this process made (ADR 0077 §2).
func TestResumeLeavesAKeyThisProcessAlreadyHolds(t *testing.T) {
	s := NewStates(time.Hour)
	s.Observe(stateT0, failing("services", nil))
	s.Observe(stateT0.Add(time.Minute), failing("services", nil))

	stale := StateRecord{
		State: StateFailing, Subject: "services",
		WindowStart: stateT0, WindowSeconds: 3600, OccupiedNanos: 999,
	}
	if seeded := s.Resume([]StateRecord{stale}); seeded != 0 {
		t.Errorf("resumed %d records over a live one, want 0", seeded)
	}
	if rec := only(t, s); rec.OccupiedNanos == 999 {
		t.Error("a record on disk displaced one this process observed")
	}
}

// A window that has ended leaves the accumulator, which is what bounds memory.
func TestCloseBeforeReleasesEndedWindows(t *testing.T) {
	s := NewStates(time.Hour)
	s.Observe(stateT0, failing("services", nil))
	s.Observe(stateT0.Add(70*time.Minute), failing("services", nil))

	closed := s.CloseBefore(stateT0.Add(80 * time.Minute))
	if len(closed) != 1 || !closed[0].WindowStart.Equal(stateT0) {
		t.Fatalf("closed %+v, want the first window alone", closed)
	}
	if got := s.Snapshots(); len(got) != 1 || got[0].WindowStart.Equal(stateT0) {
		t.Errorf("after closing, holding %+v, want the open window alone", got)
	}
}

// The halt is the other subject, and it is the one whose payload cannot arrive:
// a halted agent ships nothing, so the window is what a later reader has.
func TestTheHaltIsAccruedLikeAnyOtherState(t *testing.T) {
	s := NewStates(time.Hour)
	halted := []AgentState{{State: StateHalted, Subject: SubjectShip}}
	s.Observe(stateT0, halted)
	s.Observe(stateT0.Add(time.Minute), halted)
	since := at(time.Minute)
	s.Observe(stateT0.Add(5*time.Minute), []AgentState{
		{State: StateHalted, Subject: SubjectShip, Since: since},
	})

	rec := only(t, s)
	if rec.Entries != 1 || rec.OpenSince == nil || !rec.OpenSince.Equal(*since) {
		t.Errorf("entries %d, open_since %v, want one episode open since %s",
			rec.Entries, rec.OpenSince, since)
	}
	if want := (4 * time.Minute).Nanoseconds(); rec.OccupiedNanos != want {
		t.Errorf("occupied %d, want %d", rec.OccupiedNanos, want)
	}
}
