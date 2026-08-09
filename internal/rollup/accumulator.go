package rollup

import (
	"math"
	"math/bits"
	"sort"
	"time"
)

// Accumulator assigns observations to wall-clock-aligned windows (UTC) and
// holds the open records. It is owned by a single goroutine — the poller —
// and is not safe for concurrent use.
type Accumulator struct {
	windowLength time.Duration
	open         map[openKey]*Record
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
	return &Accumulator{windowLength: windowLength, open: map[openKey]*Record{}}
}

// record returns the open record for the window containing at, creating it
// on first touch.
func (a *Accumulator) record(k Key, at time.Time) *Record {
	start := at.UTC().Truncate(a.windowLength)
	ok := openKey{Key: k, start: start.UnixNano()}
	r := a.open[ok]
	if r == nil {
		r = newRecord(k, start, int64(a.windowLength/time.Second))
		a.open[ok] = r
	}
	return r
}

// addProRata splits a counter delta accrued over [from, to) across the
// windows the interval overlaps, pro rata by overlap, and adds each part to
// the window's record via add. Parts sum exactly to amount — the last
// segment absorbs rounding — so window totals stay exact regardless of poll
// alignment. Non-positive intervals and negative deltas are ignored: the
// delta tracker upstream handles counter resets by rebaselining, never by
// emitting them here.
func (a *Accumulator) addProRata(k Key, from, to time.Time, amount int64, add func(*Record, int64)) {
	total := to.Sub(from).Nanoseconds()
	if total <= 0 || amount < 0 {
		return
	}
	remaining := amount
	segFrom := from
	for segFrom.Before(to) {
		segTo := segFrom.UTC().Truncate(a.windowLength).Add(a.windowLength)
		part := remaining
		if segTo.Before(to) {
			part = mulDiv(amount, segTo.Sub(segFrom).Nanoseconds(), total)
			remaining -= part
		} else {
			segTo = to
		}
		add(a.record(k, segFrom), part)
		segFrom = segTo
	}
}

// ObserveCPUDelta records one CPU counter delta: coreNanos of CPU time
// consumed over [from, to). The total is split pro rata across windows; the
// rate sample for the distribution is attributed to the window holding the
// interval's last instant.
func (a *Accumulator) ObserveCPUDelta(k Key, from, to time.Time, coreNanos int64) {
	if to.Sub(from) <= 0 || coreNanos < 0 {
		return
	}
	a.addProRata(k, from, to, coreNanos, func(r *Record, v int64) { r.CPU.CoreNanoseconds += v })
	a.record(k, to.Add(-time.Nanosecond)).CPU.observeRate(mulDiv(coreNanos, 1000, to.Sub(from).Nanoseconds()))
}

// ObserveThrottling records CFS throttling counter deltas over [from, to),
// split pro rata across windows like every counter total.
func (a *Accumulator) ObserveThrottling(k Key, from, to time.Time, throttledPeriods, totalPeriods int64) {
	a.addProRata(k, from, to, throttledPeriods, func(r *Record, v int64) { r.CPU.ThrottledPeriods += v })
	a.addProRata(k, from, to, totalPeriods, func(r *Record, v int64) { r.CPU.TotalPeriods += v })
}

// ObserveCPUPSI records a PSI some-CPU stall counter delta in nanoseconds
// over [from, to).
func (a *Accumulator) ObserveCPUPSI(k Key, from, to time.Time, stallNanos int64) {
	a.addProRata(k, from, to, stallNanos, func(r *Record, v int64) { r.CPU.PSIStallNanoseconds += v })
}

// ObserveMemoryPSI records a PSI some-memory stall counter delta in
// nanoseconds over [from, to).
func (a *Accumulator) ObserveMemoryPSI(k Key, from, to time.Time, stallNanos int64) {
	a.addProRata(k, from, to, stallNanos, func(r *Record, v int64) { r.Memory.PSIStallNanoseconds += v })
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
		if !r.WindowStart.Add(a.windowLength).After(now) {
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

// mulDiv returns a*b/c without intermediate overflow, for a, b ≥ 0 and
// c > 0 (anything else returns 0), saturating at MaxInt64 when the quotient
// does not fit.
func mulDiv(a, b, c int64) int64 {
	if a < 0 || b < 0 || c <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b)) // #nosec G115 -- guarded non-negative above
	if hi >= uint64(c) {                       // #nosec G115 -- guarded positive above
		return math.MaxInt64
	}
	q, _ := bits.Div64(hi, lo, uint64(c)) // #nosec G115 -- guarded positive above
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}
