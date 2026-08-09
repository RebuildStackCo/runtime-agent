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

// ObserveCPUDelta records one CPU counter delta: coreNanos of CPU time
// consumed over [from, to). The total is split pro rata across the windows
// the interval overlaps, so window totals stay exact regardless of poll
// alignment; the rate sample for the distribution is attributed to the
// window holding the interval's last instant. Non-positive intervals and
// negative deltas are ignored — the delta tracker upstream handles counter
// resets by rebaselining, never by emitting them here.
func (a *Accumulator) ObserveCPUDelta(k Key, from, to time.Time, coreNanos int64) {
	total := to.Sub(from).Nanoseconds()
	if total <= 0 || coreNanos < 0 {
		return
	}

	remaining := coreNanos
	segFrom := from
	for segFrom.Before(to) {
		segTo := segFrom.UTC().Truncate(a.windowLength).Add(a.windowLength)
		part := remaining
		if segTo.Before(to) {
			part = mulDiv(coreNanos, segTo.Sub(segFrom).Nanoseconds(), total)
			remaining -= part
		} else {
			segTo = to // the last segment absorbs rounding, so parts sum exactly
		}
		a.record(k, segFrom).CPU.CoreNanoseconds += part
		segFrom = segTo
	}

	a.record(k, to.Add(-time.Nanosecond)).CPU.observeRate(mulDiv(coreNanos, 1000, total))
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
