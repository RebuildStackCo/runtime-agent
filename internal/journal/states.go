package journal

import (
	"sort"
	"sync"
	"time"
)

// The agent's own states, as time rather than as a flag. `collection_coverage`
// says which states hold now and since when (ADR 0087); it cannot say that a
// state ended, because it supersedes and a state that ended leaves no field
// behind. This journal is the other half: how much of each window a state
// occupied, and how many episodes made it up (ADR 0088).

// State names, and the one subject that is not a resource class.
const (
	StateFailing  = "failing"
	StateHalted   = "halted"
	SubjectShip   = "shipping"
	subjectKeySep = "\x00"
)

// AgentState is one of the agent's own states as one pass found it. Since is
// when the current episode began, and nil is the claim that the subject is not
// in this state — the same two values `collection_coverage` carries, read from
// the same call, so the window and the snapshot cannot disagree.
type AgentState struct {
	State   string
	Subject string
	Since   *time.Time
}

// StateRecord is one subject's time in one state within one window.
//
// Occupied and Observed are both needed: zero occupancy against a full window
// is a healthy hour, and zero occupancy against zero observation is an hour
// nobody watched. Without the second, the two are the same bytes — which is the
// defect ADR 0054 exists to prevent, arriving in a new shape.
type StateRecord struct {
	State   string `json:"state"`
	Subject string `json:"subject"`

	WindowStart   time.Time `json:"window_start"`
	WindowSeconds int64     `json:"window_seconds"`

	// OccupiedNanos is time in the state; ObservedNanos is time the agent was
	// running and watching this subject at all. A restart's gap is in neither.
	OccupiedNanos int64 `json:"occupied_nanos"`
	ObservedNanos int64 `json:"observed_nanos"`
	// Entries is episodes that began within this window. A state continuing
	// from the previous window contributes occupancy here and no entry, so
	// summing entries across windows counts episodes rather than windows.
	Entries int64 `json:"entries"`

	// AccruedTo is the instant the two durations above are true up to, and what
	// a later process resumes accrual from. It is never used to re-measure a
	// span already counted (ADR 0088 §3).
	AccruedTo time.Time `json:"accrued_to"`
	// OpenSince is the episode's start when the state was still open at
	// AccruedTo, and absent otherwise. It is what tells a resumed process that
	// an episode it finds open is the one already counted, not a new one.
	OpenSince *time.Time `json:"open_since,omitempty"`
}

type stateKey struct {
	state   string
	subject string
	start   int64
}

// lastSeen is what the previous pass found for one subject: how far accrual has
// reached, and which episode was open there.
type lastSeen struct {
	accruedTo time.Time
	since     *time.Time
}

// States accumulates the agent's own states into wall-clock-aligned windows,
// the same windows the usage rollups use.
type States struct {
	mu           sync.Mutex
	windowLength time.Duration
	open         map[stateKey]*StateRecord
	seen         map[string]lastSeen
}

// NewStates returns an accumulator over windows of the given length, which must
// be positive.
func NewStates(windowLength time.Duration) *States {
	if windowLength <= 0 {
		panic("journal: window length must be positive")
	}
	return &States{
		windowLength: windowLength,
		open:         map[stateKey]*StateRecord{},
		seen:         map[string]lastSeen{},
	}
}

// Observe folds the interval since the previous pass into the windows it spans,
// for every subject the pass looked at.
//
// A subject seen for the first time accrues nothing: there is no interval to
// attribute yet, and on a resumed process the span before the restart is
// already in the record on disk and must not be measured a second time.
func (s *States) Observe(at time.Time, states []AgentState) {
	at = at.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, st := range states {
		if st.State == "" || st.Subject == "" {
			continue
		}
		key := st.State + subjectKeySep + st.Subject
		prev, known := s.seen[key]
		if known && at.After(prev.accruedTo) {
			s.accrue(st, prev, at)
		}
		if st.Since != nil {
			s.enter(st, prev, known, at)
		}
		s.stamp(st, at)
		s.seen[key] = lastSeen{accruedTo: at, since: st.Since}
	}
}

// accrue attributes [prev.accruedTo, at] to the windows it spans: all of it as
// observed, and the part of it the state was open as occupied.
//
// Open at both ends means open throughout — an episode is separated from the
// next by longer than a pass (ADR 0088 §4). Open at the start and closed at the
// end attributes no occupancy at all: when it closed is not knowable from two
// readings, and guessing would be the payload's only invented number.
func (s *States) accrue(st AgentState, prev lastSeen, at time.Time) {
	from := prev.accruedTo
	var occupiedFrom time.Time
	switch {
	case st.Since == nil:
		occupiedFrom = time.Time{} // closed at this end: nothing to attribute
	case prev.since != nil:
		occupiedFrom = from // open at both ends
	default:
		occupiedFrom = maxTime(*st.Since, from) // opened within the interval
	}
	for _, span := range s.split(from, at) {
		rec := s.record(st, span.start)
		rec.ObservedNanos += span.end.Sub(span.start).Nanoseconds()
		// Each window this interval touched is now true up to its own end, not
		// up to wherever the pass happened to read the clock.
		rec.AccruedTo, rec.OpenSince = span.end, st.Since
		if occupiedFrom.IsZero() {
			continue
		}
		start := maxTime(occupiedFrom, span.start)
		if start.Before(span.end) {
			rec.OccupiedNanos += span.end.Sub(start).Nanoseconds()
		}
	}
}

// enter counts an episode, in the window where this pass first saw it open.
//
// The instant the episode began is not where it is counted: an episode that
// began just before a window closed would otherwise reopen a window already
// flushed. What it began at is in OpenSince and in the occupancy; what this
// counts is episodes, and counting one where it was seen keeps every window
// final once it has ended.
func (s *States) enter(st AgentState, prev lastSeen, known bool, at time.Time) {
	if known && prev.since != nil && prev.since.Equal(*st.Since) {
		return // the same episode, still running
	}
	rec := s.record(st, at)
	if !known && rec.OpenSince != nil && rec.OpenSince.Equal(*st.Since) {
		return // resumed: this episode was counted by the process before this one
	}
	rec.Entries++
}

// stamp records how far this subject's accrual has reached and whether it is
// still open, on the window holding at — the record a resumed process reads.
func (s *States) stamp(st AgentState, at time.Time) {
	rec := s.record(st, at)
	rec.AccruedTo = at
	rec.OpenSince = st.Since
}

// record returns the open record for one subject in the window holding at,
// creating it if this is the first thing to land there.
func (s *States) record(st AgentState, at time.Time) *StateRecord {
	start := at.UTC().Truncate(s.windowLength)
	key := stateKey{state: st.State, subject: st.Subject, start: start.UnixNano()}
	rec := s.open[key]
	if rec == nil {
		rec = &StateRecord{
			State:         st.State,
			Subject:       st.Subject,
			WindowStart:   start,
			WindowSeconds: int64(s.windowLength / time.Second),
		}
		s.open[key] = rec
	}
	return rec
}

// span is one window's share of an interval.
type span struct{ start, end time.Time }

// split cuts [from, to] at window boundaries, so an interval that crosses one
// is attributed to both windows rather than to whichever end it was read at.
func (s *States) split(from, to time.Time) []span {
	var out []span
	for from.Before(to) {
		end := from.Truncate(s.windowLength).Add(s.windowLength)
		if end.After(to) {
			end = to
		}
		out = append(out, span{start: from, end: end})
		from = end
	}
	return out
}

// Snapshots returns deep copies of every open record, sorted so the payload
// bytes are deterministic (the golden contract).
func (s *States) Snapshots() []StateRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StateRecord, 0, len(s.open))
	for _, rec := range s.open {
		out = append(out, rec.clone())
	}
	sortStateRecords(out)
	return out
}

// Resume seeds open windows from records a previous process wrote, so a restart
// continues the window rather than writing a subset of it back over itself
// (ADR 0077). A key already present is left alone.
//
// The gap between the previous process's last accrual and this one's first pass
// is attributed to nothing: the agent was not watching, and ObservedNanos is
// the number that says so.
func (s *States) Resume(records []StateRecord) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var seeded int
	for _, rec := range records {
		key := stateKey{state: rec.State, subject: rec.Subject, start: rec.WindowStart.UnixNano()}
		if _, ok := s.open[key]; ok {
			continue
		}
		resumed := rec.clone()
		s.open[key] = &resumed
		seeded++
	}
	return seeded
}

// CloseBefore removes and returns every record whose window ended at or before
// now. Removal is what bounds memory, exactly as it is for the journals beside
// this one.
func (s *States) CloseBefore(now time.Time) []StateRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []StateRecord
	for key, rec := range s.open {
		if rec.WindowStart.Add(s.windowLength).After(now) {
			continue
		}
		out = append(out, rec.clone())
		delete(s.open, key)
	}
	sortStateRecords(out)
	return out
}

func (r *StateRecord) clone() StateRecord {
	out := *r
	if r.OpenSince != nil {
		since := *r.OpenSince
		out.OpenSince = &since
	}
	return out
}

func sortStateRecords(records []StateRecord) {
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		switch {
		case !a.WindowStart.Equal(b.WindowStart):
			return a.WindowStart.Before(b.WindowStart)
		case a.State != b.State:
			return a.State < b.State
		default:
			return a.Subject < b.Subject
		}
	})
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
