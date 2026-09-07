package rollup

import (
	"encoding/json"
	"math/rand"
	"testing"
	"time"
)

var seedWindow = time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

func seedKey() Key {
	return Key{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"}
}

// randomRecord fills every field a record has from one seed, so the round trip
// below is not exercised on a hand-picked shape.
func randomRecord(rng *rand.Rand) *Record {
	a := NewAccumulator(time.Hour)
	k := seedKey()
	at := seedWindow
	for range 1 + rng.Intn(20) {
		next := at.Add(time.Duration(1+rng.Intn(120)) * time.Second)
		a.ObserveCPUDelta(k, at, next, rng.Int63n(60e9))
		a.ObserveThrottling(k, at, next, rng.Int63n(50), rng.Int63n(2000))
		a.ObserveCPUPSI(k, at, next, rng.Int63n(1e9))
		a.ObserveMemoryPSI(k, at, next, rng.Int63n(1e9))
		a.ObserveMemory(k, next, rng.Int63n(4<<30))
		at = next
	}
	return a.Snapshots()[0]
}

// The payload bytes are the only channel a restart has to its own history, so
// a record must survive the trip through them with nothing dropped and nothing
// approximated. Generated inputs rather than examples, like the merge property
// this recovery leans on.
func TestARecordSurvivesTheRoundTripThroughItsPayloadBytes(t *testing.T) {
	rng := rand.New(rand.NewSource(0xC0FFEE)) // #nosec G404 -- test input generation
	for i := range 200 {
		want := randomRecord(rng)
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("case %d: marshal: %v", i, err)
		}
		var got Record
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("case %d: unmarshal: %v", i, err)
		}
		again, err := json.Marshal(&got)
		if err != nil {
			t.Fatalf("case %d: remarshal: %v", i, err)
		}
		if string(again) != string(raw) {
			t.Fatalf("case %d: the record does not survive its own bytes\n got %s\nwant %s", i, again, raw)
		}
	}
}

// A histogram decoded into a value that carries no grid would silently land its
// counts on the wrong buckets, so it is refused instead.
func TestAHistogramWillNotDecodeWithoutItsGrid(t *testing.T) {
	var h Histogram
	if err := json.Unmarshal([]byte(`{"0":1}`), &h); err == nil {
		t.Fatal("decoded a histogram with no grid, want a refusal")
	}
}

// Seeding is the merge property applied to the agent's own history: a window
// split across two processes must equal the same window observed by one.
func TestSeedingAWindowEqualsObservingItInOneProcess(t *testing.T) {
	k := seedKey()
	whole := NewAccumulator(time.Hour)
	whole.ObserveCPUDelta(k, seedWindow, seedWindow.Add(30*time.Minute), 90e9)
	whole.ObserveMemory(k, seedWindow.Add(10*time.Minute), 64<<20)
	whole.ObserveCPUDelta(k, seedWindow.Add(30*time.Minute), seedWindow.Add(50*time.Minute), 60e9)
	whole.ObserveMemory(k, seedWindow.Add(40*time.Minute), 96<<20)

	first := NewAccumulator(time.Hour)
	first.ObserveCPUDelta(k, seedWindow, seedWindow.Add(30*time.Minute), 90e9)
	first.ObserveMemory(k, seedWindow.Add(10*time.Minute), 64<<20)

	second := NewAccumulator(time.Hour)
	if err := second.Seed(first.Snapshots()[0]); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	second.ObserveCPUDelta(k, seedWindow.Add(30*time.Minute), seedWindow.Add(50*time.Minute), 60e9)
	second.ObserveMemory(k, seedWindow.Add(40*time.Minute), 96<<20)

	wantBytes, _ := json.Marshal(whole.Snapshots())
	gotBytes, _ := json.Marshal(second.Snapshots())
	if string(gotBytes) != string(wantBytes) {
		t.Errorf("a window resumed mid-way differs from the same window observed whole\n got %s\nwant %s",
			gotBytes, wantBytes)
	}
}

// A record this accumulator could not have produced is refused rather than
// merged into a window it does not describe.
func TestSeedRefusesARecordFromAnotherGrid(t *testing.T) {
	cases := []struct {
		name   string
		record func() *Record
	}{
		{
			name:   "another window length",
			record: func() *Record { return newRecord(seedKey(), seedWindow, 900) },
		},
		{
			name:   "a start off the grid",
			record: func() *Record { return newRecord(seedKey(), seedWindow.Add(7*time.Minute), 3600) },
		},
		{
			name: "no histogram to merge onto",
			record: func() *Record {
				r := newRecord(seedKey(), seedWindow, 3600)
				r.CPU.Hist = nil
				return r
			},
		},
	}
	for _, c := range cases {
		a := NewAccumulator(time.Hour)
		if err := a.Seed(c.record()); err == nil {
			t.Errorf("%s: seeded without complaint, want a refusal", c.name)
		}
		if len(a.Snapshots()) != 0 {
			t.Errorf("%s: a refused record still opened a window", c.name)
		}
	}
}
