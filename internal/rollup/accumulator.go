package rollup

import (
	"sort"
	"time"
)

// Accumulator assigns observations to wall-clock-aligned windows (UTC) and
// holds the open records. It is owned by a single goroutine — the poller —
// and is not safe for concurrent use.
type Accumulator struct {
	win  windowing
	open map[openKey]*Record
}

type openKey struct {
	Key
	start int64 // window start, unix nanoseconds
}

// NewAccumulator returns an accumulator over windows of the given length,
// which must be positive.
func NewAccumulator(windowLength time.Duration) *Accumulator {
	if windowLength <= 0 {
		panic("rollup: window length must be positive")
	}
	return &Accumulator{win: windowing{length: windowLength}, open: map[openKey]*Record{}}
}

// record returns the open record for the window containing at, creating it
// on first touch.
func (a *Accumulator) record(k Key, at time.Time) *Record {
	return a.recordAt(k, a.win.startOf(at))
}

// recordAt is record for a start already aligned to a window boundary.
func (a *Accumulator) recordAt(k Key, start time.Time) *Record {
	ok := openKey{Key: k, start: start.UnixNano()}
	r := a.open[ok]
	if r == nil {
		r = newRecord(k, start, a.win.seconds())
		a.open[ok] = r
	}
	return r
}

// addProRata splits a counter delta accrued over [from, to) across the windows
// the interval overlaps, adding each part to that window's record via add. The
// arithmetic is windowing.split's; what belongs here is only which record each
// part lands in.
func (a *Accumulator) addProRata(k Key, from, to time.Time, amount int64, add func(*Record, int64)) {
	a.win.split(from, to, amount, func(start time.Time, part int64) {
		add(a.recordAt(k, start), part)
	})
}

// ObserveCPUDelta records one CPU counter delta: coreNanos of CPU time
// consumed over [from, to). The total is split pro rata across windows; the
// rate sample for the distribution is attributed to the window holding the
// interval's last instant.
// The interval's own duration is split the same way into CoveredNanoseconds,
// so each window records exactly how much of itself this container was
// observed for.
func (a *Accumulator) ObserveCPUDelta(k Key, from, to time.Time, coreNanos int64) {
	if to.Sub(from) <= 0 || coreNanos < 0 {
		return
	}
	a.addProRata(k, from, to, coreNanos, func(r *Record, v int64) { r.CPU.CoreNanoseconds += v })
	a.addProRata(k, from, to, to.Sub(from).Nanoseconds(), func(r *Record, v int64) { r.CPU.CoveredNanoseconds += v })
	a.record(k, to.Add(-time.Nanosecond)).CPU.observeRate(mulDiv(coreNanos, 1000, to.Sub(from).Nanoseconds()))
}

// ObserveThrottling records CFS throttling counter deltas over [from, to),
// split pro rata across windows like every counter total. The observation
// itself — the fact that a scrape carried these counters at all — is counted
// once, in the window holding the interval's last instant, exactly as a rate
// sample is. That count is what separates "did not throttle" from "was never
// observed" downstream.
func (a *Accumulator) ObserveThrottling(k Key, from, to time.Time, throttledPeriods, totalPeriods int64) {
	a.addProRata(k, from, to, throttledPeriods, func(r *Record, v int64) { r.CPU.ThrottledPeriods += v })
	a.addProRata(k, from, to, totalPeriods, func(r *Record, v int64) { r.CPU.TotalPeriods += v })
	if to.After(from) {
		a.record(k, to.Add(-time.Nanosecond)).CPU.ThrottlingSamples++
	}
}

// ObserveCPUPSI records a PSI some-CPU stall counter delta in nanoseconds
// over [from, to), and counts the observation so a zero stall is
// distinguishable from an unexposed one.
func (a *Accumulator) ObserveCPUPSI(k Key, from, to time.Time, stallNanos int64) {
	a.addProRata(k, from, to, stallNanos, func(r *Record, v int64) { r.CPU.PSIStallNanoseconds += v })
	if to.After(from) {
		a.record(k, to.Add(-time.Nanosecond)).CPU.PSISamples++
	}
}

// ObserveMemoryPSI records a PSI some-memory stall counter delta in
// nanoseconds over [from, to), counting the observation likewise.
func (a *Accumulator) ObserveMemoryPSI(k Key, from, to time.Time, stallNanos int64) {
	a.addProRata(k, from, to, stallNanos, func(r *Record, v int64) { r.Memory.PSIStallNanoseconds += v })
	if to.After(from) {
		a.record(k, to.Add(-time.Nanosecond)).Memory.PSISamples++
	}
}

// ObserveMemory records one working-set sample in bytes, taken at the given
// time. Negative values are ignored.
func (a *Accumulator) ObserveMemory(k Key, at time.Time, workingSetBytes int64) {
	if workingSetBytes < 0 {
		return
	}
	a.record(k, at).Memory.observe(workingSetBytes)
}

// CloseBefore removes and returns every record whose window ended at or
// before now, sorted deterministically.
func (a *Accumulator) CloseBefore(now time.Time) []*Record {
	var out []*Record
	for ok, r := range a.open {
		if !r.WindowStart.Add(a.win.length).After(now) {
			out = append(out, r)
			delete(a.open, ok)
		}
	}
	sortRecords(out)
	return out
}

// Snapshots returns deep copies of all open records, sorted
// deterministically — the periodic open-window snapshot of ADR 0006.
func (a *Accumulator) Snapshots() []*Record {
	out := make([]*Record, 0, len(a.open))
	for _, r := range a.open {
		out = append(out, r.clone())
	}
	sortRecords(out)
	return out
}

func sortRecords(rs []*Record) {
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if !a.WindowStart.Equal(b.WindowStart) {
			return a.WindowStart.Before(b.WindowStart)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.WorkloadKind != b.WorkloadKind {
			return a.WorkloadKind < b.WorkloadKind
		}
		if a.WorkloadName != b.WorkloadName {
			return a.WorkloadName < b.WorkloadName
		}
		return a.Container < b.Container
	})
}
