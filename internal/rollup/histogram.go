// Package rollup implements the mergeable usage rollups of ADR 0006:
// fixed-boundary logarithmic histograms, exact window totals, and
// wall-clock-aligned window accumulation. Merging N partial rollups equals
// one rollup over the union of their inputs — the merge property everything
// downstream relies on (docs/development.md).
package rollup

import (
	"bytes"
	"fmt"
	"maps"
	"math/bits"
	"sort"
	"strconv"
)

// The bucket grid is frozen. Boundaries follow anchor·2^(i/8) — ~9% growth
// per bucket, which bounds any percentile read from a histogram to ~±4.5% of
// the true value. octaveSteps holds 2^(m/8) for m = 0..7 in 32.32 fixed
// point, so boundaries are exact integers, identical on every platform.
// Together with the anchors below, these constants are the payload schema:
// changing any of them breaks mergeability with all shipped history and is a
// schema version change (ADR 0006), never a silent edit.
var octaveSteps = [8]uint64{
	4294967296, // 2^(0/8)
	4683695048, // 2^(1/8)
	5107605667, // 2^(2/8)
	5569883475, // 2^(3/8)
	6074001000, // 2^(4/8)
	6623745059, // 2^(5/8)
	7223245206, // 2^(6/8)
	7877004752, // 2^(7/8)
}

// fixedShift is the fractional width of the octaveSteps fixed-point values.
const fixedShift = 32

// Histogram anchors: the lower boundary of bucket 1, as log2 of the value in
// the unit the histogram counts. Bucket 0 is the underflow bucket [0, anchor).
const (
	cpuAnchorShift    = 0  // CPU histograms count millicores; anchor 1 millicore
	memoryAnchorShift = 20 // memory histograms count bytes; anchor 1 MiB
)

// Histogram is a sparse fixed-boundary logarithmic histogram. Bucket 0
// covers [0, anchor); bucket i ≥ 1 covers values v with
// LowerBound(i) ≤ v < UpperBound(i). At small values the integer boundaries
// coincide and the effective resolution becomes exact single units; the
// zero-width bucket indices this creates are simply never occupied.
//
// The zero value is not usable; construct with NewCPUHistogram or
// NewMemoryHistogram.
type Histogram struct {
	anchorShift int
	counts      map[int]uint64
}

// NewCPUHistogram returns a histogram of CPU rates in millicores.
func NewCPUHistogram() *Histogram {
	return &Histogram{anchorShift: cpuAnchorShift, counts: map[int]uint64{}}
}

// NewMemoryHistogram returns a histogram of memory sizes in bytes.
func NewMemoryHistogram() *Histogram {
	return &Histogram{anchorShift: memoryAnchorShift, counts: map[int]uint64{}}
}

// bound returns the frozen boundary anchor·2^(j/8) for j ≥ 0, saturating at
// the top of the uint64 range where the grid exceeds representable values.
func (h *Histogram) bound(j int) uint64 {
	if j < 0 {
		return 0
	}
	shift := h.anchorShift + j/8
	if shift > 62 {
		return ^uint64(0)
	}
	hi, lo := bits.Mul64(uint64(1)<<shift, octaveSteps[j%8])
	return hi<<(64-fixedShift) | lo>>fixedShift
}

// Index returns the bucket for a value: the largest j with
// anchor·2^(j/8) ≤ v, plus one; values below the anchor (including any
// negative input, which cannot occur from real counters) land in the
// underflow bucket 0.
func (h *Histogram) Index(v int64) int {
	if v < 1<<h.anchorShift {
		return 0
	}
	u := uint64(v) // #nosec G115 -- v ≥ anchor > 0, checked above
	q := bits.Len64(u>>h.anchorShift) - 1
	for m := 7; m > 0; m-- {
		j := 8*q + m
		if h.bound(j) <= u {
			return j + 1
		}
	}
	return 8*q + 1 // bound(8q) = anchor·2^q ≤ v by construction of q
}

// LowerBound returns the smallest value that falls into bucket i.
func (h *Histogram) LowerBound(i int) uint64 {
	if i <= 0 {
		return 0
	}
	return h.bound(i - 1)
}

// UpperBound returns the exclusive upper edge of bucket i.
func (h *Histogram) UpperBound(i int) uint64 {
	return h.bound(i)
}

// Observe adds one occurrence of v.
func (h *Histogram) Observe(v int64) {
	h.counts[h.Index(v)]++
}

// Count returns the total number of observations.
func (h *Histogram) Count() uint64 {
	var n uint64
	for _, c := range h.counts {
		n += c
	}
	return n
}

// Counts returns a copy of the occupied buckets, keyed by bucket index.
func (h *Histogram) Counts() map[int]uint64 {
	out := make(map[int]uint64, len(h.counts))
	maps.Copy(out, h.counts)
	return out
}

// Merge folds o into h. Both histograms must be on the same grid.
func (h *Histogram) Merge(o *Histogram) error {
	if h.anchorShift != o.anchorShift {
		return fmt.Errorf("rollup: merging histograms with different anchors")
	}
	for i, c := range o.counts {
		if c != 0 {
			h.counts[i] += c
		}
	}
	return nil
}

func (h *Histogram) clone() *Histogram {
	c := &Histogram{anchorShift: h.anchorShift, counts: make(map[int]uint64, len(h.counts))}
	maps.Copy(c.counts, h.counts)
	return c
}

// MarshalJSON encodes the sparse buckets as {"<lower bound>": count} with
// keys in ascending value order, so identical histograms marshal to
// identical bytes. Occupied buckets always have distinct lower bounds.
func (h *Histogram) MarshalJSON() ([]byte, error) {
	idx := make([]int, 0, len(h.counts))
	for i := range h.counts {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var b bytes.Buffer
	b.WriteByte('{')
	for n, i := range idx {
		if n > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strconv.FormatUint(h.LowerBound(i), 10))
		b.WriteString(`":`)
		b.WriteString(strconv.FormatUint(h.counts[i], 10))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
