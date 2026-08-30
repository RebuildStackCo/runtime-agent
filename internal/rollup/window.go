package rollup

import (
	"math"
	"math/bits"
	"time"
)

// windowing is the wall-clock alignment every rollup in this package shares:
// records belong to UTC-aligned windows of one length, and a counter delta that
// spans a boundary is divided between the windows it covers.
//
// It is one type rather than a copy per accumulator because the merge property
// the backend relies on — shorter windows fold into longer ones losslessly
// (ADR 0006) — rests on this arithmetic, and it is tested once.
type windowing struct{ length time.Duration }

func (w windowing) startOf(at time.Time) time.Time { return at.UTC().Truncate(w.length) }

func (w windowing) seconds() int64 { return int64(w.length / time.Second) }

// split calls yield once per window the interval [from, to) overlaps, with that
// window's start and its pro-rata share of amount. The parts sum to exactly
// amount — the last segment absorbs the rounding — so a window total is right
// however the polls happen to land.
//
// A non-positive interval or a negative delta yields nothing: the counter
// trackers upstream deal with resets by rebaselining, never by handing a
// negative delta down here.
func (w windowing) split(from, to time.Time, amount int64, yield func(start time.Time, part int64)) {
	total := to.Sub(from).Nanoseconds()
	if total <= 0 || amount < 0 {
		return
	}
	remaining := amount
	segFrom := from
	for segFrom.Before(to) {
		segTo := w.startOf(segFrom).Add(w.length)
		part := remaining
		if segTo.Before(to) {
			part = mulDiv(amount, segTo.Sub(segFrom).Nanoseconds(), total)
			remaining -= part
		} else {
			segTo = to
		}
		yield(w.startOf(segFrom), part)
		segFrom = segTo
	}
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
