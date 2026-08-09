package rollup

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand"
	"strconv"
	"testing"
)

func TestBucketBoundsAreFrozenLogGrid(t *testing.T) {
	for _, h := range []*Histogram{NewCPUHistogram(), NewMemoryHistogram()} {
		// One octave up is a doubling. The boundaries are floors of the
		// exact grid, and floor(2x) is 2·floor(x) or 2·floor(x)+1 — never
		// anything else.
		for j := range 200 {
			got, twice := h.bound(j+8), 2*h.bound(j)
			if got != twice && got != twice+1 {
				t.Fatalf("bound(%d) = %d, want 2*bound(%d) = %d (±1 from flooring)", j+8, got, j, twice)
			}
		}
		// Within an octave the growth approximates 2^(1/8) once values are
		// large enough for integer boundaries to resolve the ratio.
		const step = 1.0905077326652577
		for j := 250; j < 320; j++ {
			ratio := float64(h.bound(j+1)) / float64(h.bound(j))
			if math.Abs(ratio-step) > 1e-6 {
				t.Fatalf("bound(%d)/bound(%d) = %v, want ~%v", j+1, j, ratio, step)
			}
		}
	}
}

func TestIndexBracketsValue(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, h := range []*Histogram{NewCPUHistogram(), NewMemoryHistogram()} {
		values := []int64{0, 1, 2, 3, (1 << 20) - 1, 1 << 20, 1<<20 + 1, math.MaxInt64}
		for range 10000 {
			values = append(values, rng.Int63n(1<<(10+rng.Intn(50))))
		}
		for _, v := range values {
			i := h.Index(v)
			if i < 0 {
				t.Fatalf("Index(%d) = %d, negative bucket", v, i)
			}
			lo, hi := h.LowerBound(i), h.UpperBound(i)
			u := uint64(v) // #nosec G115 -- generated values are non-negative
			if u < lo || u >= hi {
				t.Fatalf("value %d in bucket %d, but bucket is [%d, %d)", v, i, lo, hi)
			}
		}
	}
}

func TestIndexRelativeErrorBound(t *testing.T) {
	// The promise of the 2^(1/8) grid: the geometric midpoint of a bucket is
	// within ~±4.5% of any value in it (ADR 0006).
	rng := rand.New(rand.NewSource(2))
	h := NewMemoryHistogram()
	for range 10000 {
		v := 1<<20 + rng.Int63n(1<<40)
		i := h.Index(v)
		mid := math.Sqrt(float64(h.LowerBound(i)) * float64(h.UpperBound(i)))
		if err := math.Abs(mid-float64(v)) / float64(v); err > 0.045 {
			t.Fatalf("value %d: bucket midpoint %v is off by %.4f, want ≤ 0.045", v, mid, err)
		}
	}
}

func TestHistogramMergeAddsCounts(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	a, b, all := NewCPUHistogram(), NewCPUHistogram(), NewCPUHistogram()
	for i := range 5000 {
		v := rng.Int63n(1 << 30)
		all.Observe(v)
		if i%2 == 0 {
			a.Observe(v)
		} else {
			b.Observe(v)
		}
	}
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if got, want := a.Count(), all.Count(); got != want {
		t.Fatalf("merged count %d, want %d", got, want)
	}
	got, want := a.Counts(), all.Counts()
	if len(got) != len(want) {
		t.Fatalf("merged has %d occupied buckets, want %d", len(got), len(want))
	}
	for i, c := range want {
		if got[i] != c {
			t.Fatalf("bucket %d: merged count %d, want %d", i, got[i], c)
		}
	}
}

func TestHistogramMergeRejectsMixedGrids(t *testing.T) {
	if err := NewCPUHistogram().Merge(NewMemoryHistogram()); err == nil {
		t.Fatal("merging CPU and memory histograms must fail")
	}
}

func TestHistogramMarshalIsDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	h := NewMemoryHistogram()
	for range 1000 {
		h.Observe(rng.Int63n(1 << 34))
	}
	first, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("marshaling the same histogram twice produced different bytes")
	}

	// Keys are lower bounds in ascending order, counts sum to the total.
	var decoded map[string]uint64
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("marshaled histogram is not a JSON object: %v", err)
	}
	var sum uint64
	for _, c := range decoded {
		sum += c
	}
	if sum != h.Count() {
		t.Fatalf("marshaled counts sum to %d, want %d", sum, h.Count())
	}
	var prev int64 = -1
	for _, k := range jsonKeysInOrder(t, first) {
		n, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			t.Fatalf("bucket key %q is not an integer: %v", k, err)
		}
		if n <= prev {
			t.Fatalf("bucket keys not in ascending order: %d after %d", n, prev)
		}
		prev = n
	}
}

// jsonKeysInOrder returns the object's keys in the order they appear in the
// encoded bytes, which json.Unmarshal into a map would discard.
func jsonKeysInOrder(t *testing.T, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	var keys []string
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch tok := tok.(type) {
		case json.Delim:
			if tok == '{' || tok == '[' {
				depth++
			} else {
				depth--
			}
		case string:
			// All values in a histogram object are numbers, so every
			// string token at depth 1 is a key.
			if depth == 1 {
				keys = append(keys, tok)
			}
		}
	}
	return keys
}
