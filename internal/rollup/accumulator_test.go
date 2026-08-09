package rollup

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// The merge property (docs/development.md): merging N partial rollups must
// equal computing one rollup over the union of their inputs, and merging
// must be associative and commutative. These are tested with generated
// inputs — mergeability is what the whole data model rests on.

var testWindow = time.Hour

// observation is one poller fact, applicable to any accumulator.
type observation func(*Accumulator)

func genObservations(rng *rand.Rand, n int) []observation {
	keys := []Key{
		{Namespace: "payments", WorkloadKind: "Deployment", WorkloadName: "api", Container: "app"},
		{Namespace: "payments", WorkloadKind: "Deployment", WorkloadName: "api", Container: "sidecar"},
		{Namespace: "search", WorkloadKind: "StatefulSet", WorkloadName: "index", Container: "app"},
		{Namespace: "batch", WorkloadKind: "Job", WorkloadName: "reindex-42", Container: "worker"},
	}
	base := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)
	out := make([]observation, 0, n)
	for range n {
		k := keys[rng.Intn(len(keys))]
		at := base.Add(time.Duration(rng.Int63n(int64(4 * time.Hour))))
		if rng.Intn(2) == 0 {
			// A poll-interval CPU delta, sometimes spanning a window edge.
			dur := time.Duration(15+rng.Intn(90)) * time.Second
			coreNanos := rng.Int63n(int64(dur) * 8) // up to 8 cores
			out = append(out, func(a *Accumulator) {
				a.ObserveCPUDelta(k, at, at.Add(dur), coreNanos)
			})
		} else {
			bytes := rng.Int63n(8 << 30)
			out = append(out, func(a *Accumulator) {
				a.ObserveMemory(k, at, bytes)
			})
		}
	}
	return out
}

// drain returns all records of an accumulator, closed and sorted.
func drain(a *Accumulator) []*Record {
	return a.CloseBefore(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
}

func genRecord(rng *rand.Rand) *Record {
	a := NewAccumulator(testWindow)
	for _, apply := range genObservations(rng, 50) {
		apply(a)
	}
	all := drain(a)
	return all[rng.Intn(len(all))]
}

// sameIdentity re-keys a record generated independently so merge accepts it.
func sameIdentity(r, as *Record) *Record {
	r.Key = as.Key
	r.WindowStart = as.WindowStart
	r.WindowSeconds = as.WindowSeconds
	return r
}

func TestMergeCommutative(t *testing.T) {
	for seed := range int64(20) {
		rng := rand.New(rand.NewSource(seed))
		a := genRecord(rng)
		b := sameIdentity(genRecord(rng), a)

		ab := a.clone()
		if err := ab.Merge(b); err != nil {
			t.Fatal(err)
		}
		ba := b.clone()
		if err := ba.Merge(a); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ab, ba) {
			t.Fatalf("seed %d: merge is not commutative:\na·b = %+v\nb·a = %+v", seed, ab, ba)
		}
	}
}

func TestMergeAssociative(t *testing.T) {
	for seed := range int64(20) {
		rng := rand.New(rand.NewSource(100 + seed))
		a := genRecord(rng)
		b := sameIdentity(genRecord(rng), a)
		c := sameIdentity(genRecord(rng), a)

		left := a.clone()
		if err := left.Merge(b); err != nil {
			t.Fatal(err)
		}
		if err := left.Merge(c); err != nil {
			t.Fatal(err)
		}

		bc := b.clone()
		if err := bc.Merge(c); err != nil {
			t.Fatal(err)
		}
		right := a.clone()
		if err := right.Merge(bc); err != nil {
			t.Fatal(err)
		}

		if !reflect.DeepEqual(left, right) {
			t.Fatalf("seed %d: merge is not associative:\n(a·b)·c = %+v\na·(b·c) = %+v", seed, left, right)
		}
	}
}

func TestMergeRejectsDifferentIdentities(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	a := genRecord(rng)
	b := a.clone()
	b.Container = "other"
	if err := a.Merge(b); err == nil {
		t.Fatal("merging records with different keys must fail")
	}
	c := a.clone()
	c.WindowStart = c.WindowStart.Add(testWindow)
	if err := a.Merge(c); err == nil {
		t.Fatal("merging records from different windows must fail")
	}
}

func TestPartitionedRollupEqualsWholeRollup(t *testing.T) {
	for seed := range int64(10) {
		rng := rand.New(rand.NewSource(200 + seed))
		observations := genObservations(rng, 500)

		whole := NewAccumulator(testWindow)
		parts := []*Accumulator{
			NewAccumulator(testWindow),
			NewAccumulator(testWindow),
			NewAccumulator(testWindow),
		}
		for _, apply := range observations {
			apply(whole)
			apply(parts[rng.Intn(len(parts))])
		}

		merged := map[string]*Record{}
		for _, part := range parts {
			for _, r := range drain(part) {
				id := fmt.Sprintf("%v/%d", r.Key, r.WindowStart.UnixNano())
				if have := merged[id]; have != nil {
					if err := have.Merge(r); err != nil {
						t.Fatal(err)
					}
				} else {
					merged[id] = r
				}
			}
		}

		for _, want := range drain(whole) {
			id := fmt.Sprintf("%v/%d", want.Key, want.WindowStart.UnixNano())
			got := merged[id]
			if got == nil {
				t.Fatalf("seed %d: record %s missing from merged partitions", seed, id)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d: record %s differs:\nmerged = %+v\nwhole  = %+v", seed, id, got, want)
			}
			delete(merged, id)
		}
		if len(merged) != 0 {
			t.Fatalf("seed %d: merged partitions have %d extra records", seed, len(merged))
		}
	}
}

func TestCPUDeltaSplitsExactlyAcrossWindows(t *testing.T) {
	a := NewAccumulator(testWindow)
	k := Key{Namespace: "n", WorkloadKind: "Deployment", WorkloadName: "w", Container: "c"}
	from := time.Date(2026, 8, 6, 9, 59, 30, 0, time.UTC)
	to := time.Date(2026, 8, 6, 10, 0, 30, 0, time.UTC)
	a.ObserveCPUDelta(k, from, to, 60e9) // one core-minute, half in each window

	records := drain(a)
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 (the delta spans a window boundary)", len(records))
	}
	first, second := records[0], records[1]
	if got := first.CPU.CoreNanoseconds; got != 30e9 {
		t.Fatalf("window %v got %d core-nanos, want 30e9", first.WindowStart, got)
	}
	if got := second.CPU.CoreNanoseconds; got != 30e9 {
		t.Fatalf("window %v got %d core-nanos, want 30e9", second.WindowStart, got)
	}
	// The rate sample belongs to the window holding the interval's last
	// instant; the rate is one core = 1000 millicores.
	if first.CPU.Samples != 0 || second.CPU.Samples != 1 {
		t.Fatalf("samples split %d/%d, want 0/1", first.CPU.Samples, second.CPU.Samples)
	}
	if got := second.CPU.MaxMilli; got != 1000 {
		t.Fatalf("sampled rate %d milli, want 1000", got)
	}
}

func TestWindowTotalsPreserveEveryCoreNanosecond(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	a := NewAccumulator(testWindow)
	k := Key{Namespace: "n", WorkloadKind: "Deployment", WorkloadName: "w", Container: "c"}
	base := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)

	var want int64
	for range 1000 {
		from := base.Add(time.Duration(rng.Int63n(int64(5 * time.Hour))))
		dur := time.Duration(1+rng.Intn(7200)) * time.Second // up to two hours
		coreNanos := rng.Int63n(int64(dur))
		a.ObserveCPUDelta(k, from, from.Add(dur), coreNanos)
		want += coreNanos
	}

	var got int64
	for _, r := range drain(a) {
		got += r.CPU.CoreNanoseconds
	}
	if got != want {
		t.Fatalf("window totals sum to %d core-nanos, want exactly %d", got, want)
	}
}

func TestSnapshotsAreDeepCopies(t *testing.T) {
	a := NewAccumulator(testWindow)
	k := Key{Namespace: "n", WorkloadKind: "Deployment", WorkloadName: "w", Container: "c"}
	at := time.Date(2026, 8, 6, 9, 10, 0, 0, time.UTC)
	a.ObserveMemory(k, at, 512<<20)

	snap := a.Snapshots()
	if len(snap) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snap))
	}
	before := snap[0].Memory.Samples
	beforeBuckets := len(snap[0].Memory.Hist.Counts())

	a.ObserveMemory(k, at.Add(30*time.Second), 4<<30)

	if snap[0].Memory.Samples != before || len(snap[0].Memory.Hist.Counts()) != beforeBuckets {
		t.Fatal("snapshot changed after further observation — not a deep copy")
	}
}

func TestCloseBeforeClosesOnlyEndedWindows(t *testing.T) {
	a := NewAccumulator(testWindow)
	k := Key{Namespace: "n", WorkloadKind: "Deployment", WorkloadName: "w", Container: "c"}
	a.ObserveMemory(k, time.Date(2026, 8, 6, 9, 30, 0, 0, time.UTC), 1<<20)
	a.ObserveMemory(k, time.Date(2026, 8, 6, 10, 30, 0, 0, time.UTC), 1<<20)

	closed := a.CloseBefore(time.Date(2026, 8, 6, 10, 59, 0, 0, time.UTC))
	if len(closed) != 1 {
		t.Fatalf("closed %d records, want 1 (the 10:00 window is still open)", len(closed))
	}
	if got, want := closed[0].WindowStart, time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("closed window starts at %v, want %v", got, want)
	}
	if remaining := a.Snapshots(); len(remaining) != 1 {
		t.Fatalf("%d windows remain open, want 1", len(remaining))
	}
}
