package sink

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

// The cadence is a property of a payload kind, and these are the checks that
// keep the property and the behavior from parting company (ADR 0073 §5). The
// registry says a kind is written when it changes; what a test can add is that
// an unchanged one is actually held back, that a changed one actually lands,
// and that the ceiling actually fires — per kind, over the writers that ship.

// clock is a spool's clock under a test's control, so a floor or a ceiling can
// be crossed without waiting for one.
type clock struct{ at time.Time }

func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }

// driveClock replaces the spool's clock and returns the handle to it.
func driveClock(s *Spool, at time.Time) *clock {
	c := &clock{at: at}
	s.now = func() time.Time { return c.at }
	return c
}

// ignoreCadence hands the spool a clock that leaps an hour between reads, so a
// test about what the spool holds is not also a test of when it writes.
func ignoreCadence(s *Spool) {
	at := capturedAt
	s.now = func() time.Time {
		at = at.Add(time.Hour)
		return at
	}
}

// snapshotKind is one change-triggered kind: how to write its current state,
// how to write a different one, and the file both land in.
type snapshotKind struct {
	kind    string
	file    string
	write   func(*Spool, time.Time) error
	changed func(*Spool, time.Time) error
}

// changedKinds covers every row the registry declares TriggerChanged;
// TestEveryChangedKindIsCovered holds it to that.
func changedKinds() []snapshotKind {
	return []snapshotKind{
		{"workload_metadata", "workload-metadata.json",
			func(s *Spool, at time.Time) error { return s.WriteWorkloadMetadata(at, fixedWorkloadMetadata()) },
			func(s *Spool, at time.Time) error { return s.WriteWorkloadMetadata(at, fixedWorkloadMetadata()[:1]) }},
		{"workload_revisions", "workload-revisions.json",
			func(s *Spool, at time.Time) error { return s.WriteWorkloadRevisions(at, fixedRevisions()) },
			func(s *Spool, at time.Time) error { return s.WriteWorkloadRevisions(at, fixedRevisions()[:1]) }},
		{"workload_policy", "workload-policy.json",
			func(s *Spool, at time.Time) error { return s.WriteWorkloadPolicy(at, fixedWorkloadPolicy(), nil) },
			func(s *Spool, at time.Time) error { return s.WriteWorkloadPolicy(at, fixedWorkloadPolicy()[:1], nil) }},
		{"cluster_policy", "cluster-policy.json",
			func(s *Spool, at time.Time) error { return s.WriteClusterPolicy(at, fixedClusterPolicy(), nil) },
			func(s *Spool, at time.Time) error {
				return s.WriteClusterPolicy(at, fixedClusterPolicy(), []string{"resourcequotas"})
			}},
		{"restart_counters", "restart-counters.json",
			func(s *Spool, at time.Time) error { return s.WriteRestartCounters(at, fixedRestartCounters()) },
			func(s *Spool, at time.Time) error { return s.WriteRestartCounters(at, fixedRestartCounters()[:1]) }},
		{"node_metadata", "node-metadata.json",
			func(s *Spool, at time.Time) error { return s.WriteNodeMetadata(at, fixedNodeMetadata()) },
			func(s *Spool, at time.Time) error { return s.WriteNodeMetadata(at, fixedNodeMetadata()[:1]) }},
		{"go_inventory", "go-inventory.json",
			func(s *Spool, at time.Time) error { return s.WriteGoInventory(at, fixedCoverage(), fixedInventory()) },
			func(s *Spool, at time.Time) error {
				return s.WriteGoInventory(at, fixedCoverage(), fixedInventory()[:1])
			}},
		{"process_peaks", "process-peaks.json",
			func(s *Spool, at time.Time) error { return s.WriteProcessPeaks(at, fixedPeaks()) },
			func(s *Spool, at time.Time) error { return s.WriteProcessPeaks(at, fixedPeaks()[:1]) }},
		{"listening_ports", "listening-ports.json",
			func(s *Spool, at time.Time) error { return s.WriteListeningPorts(at, fixedPorts()) },
			func(s *Spool, at time.Time) error { return s.WriteListeningPorts(at, fixedPorts()[:1]) }},
	}
}

// The table above must cover the registry, or a kind acquires a cadence nothing
// exercises — the drift ADR 0022 built this whole apparatus against.
func TestEveryChangedKindIsCovered(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range changedKinds() {
		covered[c.kind] = true
	}
	for _, entry := range Registry() {
		if entry.Cadence.Trigger != TriggerChanged {
			continue
		}
		if !covered[entry.Kind] {
			t.Errorf("kind %q is written on change and changedKinds() does not cover it; "+
				"add a row there so its floor, its ceiling and its predicate are exercised", entry.Kind)
		}
	}
}

// capturedAtOf reads the payload's capture instant, which is how these tests
// tell a write that landed from one that was held back: it is the only field
// that moves between two writes of unchanged state.
func capturedAtOf(t *testing.T, dir, file string) time.Time {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, file)) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	var payload struct {
		CapturedAt time.Time `json:"captured_at"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	return payload.CapturedAt
}

// The saving. Past the floor, a payload the cluster has not changed is not
// written again — and the pass that assembled it says so in the counters, which
// is the only place the suppression is visible from outside.
func TestAnUnchangedSnapshotIsNotRewritten(t *testing.T) {
	for _, c := range changedKinds() {
		t.Run(c.kind, func(t *testing.T) {
			s, dir := newTestSpool(t)
			clk := driveClock(s, capturedAt)
			cadence := mustLookup(t, c.kind).Cadence

			if err := c.write(s, capturedAt); err != nil {
				t.Fatal(err)
			}
			clk.advance(cadence.Floor)
			later := capturedAt.Add(cadence.Floor)
			if err := c.write(s, later); err != nil {
				t.Fatal(err)
			}

			if got := capturedAtOf(t, dir, c.file); !got.Equal(capturedAt) {
				t.Errorf("%s was rewritten at %s though nothing in it changed; the payload's "+
					"capture instant is not part of the comparison (ADR 0073 §3)", c.file, got)
			}
			if n := s.Counters().Suppressed[c.kind]; n != 1 {
				t.Errorf("suppressed count for %q = %d, want 1", c.kind, n)
			}
		})
	}
}

// And the freshness. A change lands at the first pass at or after the floor.
func TestAChangedSnapshotIsWrittenAtTheFloor(t *testing.T) {
	for _, c := range changedKinds() {
		t.Run(c.kind, func(t *testing.T) {
			s, dir := newTestSpool(t)
			clk := driveClock(s, capturedAt)
			cadence := mustLookup(t, c.kind).Cadence

			if err := c.write(s, capturedAt); err != nil {
				t.Fatal(err)
			}
			clk.advance(cadence.Floor)
			later := capturedAt.Add(cadence.Floor)
			if err := c.changed(s, later); err != nil {
				t.Fatal(err)
			}

			if got := capturedAtOf(t, dir, c.file); !got.Equal(later) {
				t.Errorf("%s is still the payload captured at %s after the cluster changed; "+
					"a change is sent at the first pass past the floor", c.file, got)
			}
		})
	}
}

// The floor is a debounce and not only a rate limit: a change inside it waits.
// Ninety nodes arriving in twenty seconds are one send, not ninety.
func TestAChangeInsideTheFloorWaitsForIt(t *testing.T) {
	for _, c := range changedKinds() {
		t.Run(c.kind, func(t *testing.T) {
			s, dir := newTestSpool(t)
			clk := driveClock(s, capturedAt)
			cadence := mustLookup(t, c.kind).Cadence

			if err := c.write(s, capturedAt); err != nil {
				t.Fatal(err)
			}
			clk.advance(cadence.Floor - time.Second)
			if err := c.changed(s, clk.at); err != nil {
				t.Fatal(err)
			}
			if got := capturedAtOf(t, dir, c.file); !got.Equal(capturedAt) {
				t.Fatalf("%s was rewritten %s after the previous write, inside its %s floor",
					c.file, cadence.Floor-time.Second, cadence.Floor)
			}

			clk.advance(time.Second)
			if err := c.changed(s, clk.at); err != nil {
				t.Fatal(err)
			}
			if got := capturedAtOf(t, dir, c.file); !got.Equal(clk.at) {
				t.Errorf("%s is still the payload captured at %s once the floor has passed", c.file, got)
			}
		})
	}
}

// The ceiling, which is the half the whole scheme rests on: a payload nothing
// changed is written anyway, so a missed change — a predicate that was wrong,
// a shape that moved — stands for one ceiling and not forever (ADR 0027).
func TestAnUnchangedSnapshotIsRewrittenAtTheCeiling(t *testing.T) {
	for _, c := range changedKinds() {
		t.Run(c.kind, func(t *testing.T) {
			s, dir := newTestSpool(t)
			clk := driveClock(s, capturedAt)
			cadence := mustLookup(t, c.kind).Cadence

			if err := c.write(s, capturedAt); err != nil {
				t.Fatal(err)
			}
			clk.advance(cadence.Ceiling)
			later := capturedAt.Add(cadence.Ceiling)
			if err := c.write(s, later); err != nil {
				t.Fatal(err)
			}

			if got := capturedAtOf(t, dir, c.file); !got.Equal(later) {
				t.Errorf("%s was not rewritten after its %s ceiling; without that write a change "+
					"the predicate missed would stand forever", c.file, cadence.Ceiling)
			}
		})
	}
}

// The kinds that are not change-triggered never consult the gate. Coverage in
// particular: its freshness *is* the liveness signal, so a suppressed coverage
// payload would be the agent reporting itself dead (ADR 0054 §5).
func TestFixedAndEventKindsAreNeverSuppressed(t *testing.T) {
	s, dir := newTestSpool(t)
	driveClock(s, capturedAt)

	write := func(at time.Time) {
		t.Helper()
		if err := s.WriteCollectionCoverage(at, capturedAt, AgentInfo{Version: "test"},
			nil, model.Coverage{}, model.PlacementDrops{}, model.NodeDrops{},
			nil, nil, nil, nil, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	write(capturedAt)
	later := capturedAt.Add(time.Second)
	write(later)

	if got := capturedAtOf(t, dir, "collection-coverage.json"); !got.Equal(later) {
		t.Errorf("collection coverage was captured at %s, want %s; the agent's own freshness "+
			"is never held back", got, later)
	}
	if n := s.Counters().Suppressed["collection_coverage"]; n != 0 {
		t.Errorf("collection_coverage suppressed %d times, want 0", n)
	}
}

// A restart has no gate to consult, so the first pass of a fresh process states
// the cluster it found rather than comparing against a memory it does not have.
func TestTheFirstWriteOfAKeyAlwaysLands(t *testing.T) {
	for _, c := range changedKinds() {
		t.Run(c.kind, func(t *testing.T) {
			s, dir := newTestSpool(t)
			driveClock(s, capturedAt)
			if err := c.write(s, capturedAt); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, c.file)); err != nil {
				t.Errorf("%s was not written on the first pass: %v", c.file, err)
			}
		})
	}
}

// What the comparison sees. `captured_at` is elided because it dates the write
// and not the state; everything else is compared, so no field can quietly stop
// being watched.
func TestTheComparisonElidesTheCaptureInstantAndNothingElse(t *testing.T) {
	base := []byte(`{
  "kind": "node_metadata",
  "captured_at": "2026-08-06T10:12:00Z",
  "nodes": [
    {
      "name": "node-1",
      "captured_at": "2026-08-06T10:12:00Z"
    }
  ]
}
`)
	restamped := []byte(`{
  "kind": "node_metadata",
  "captured_at": "2026-08-06T10:13:00Z",
  "nodes": [
    {
      "name": "node-1",
      "captured_at": "2026-08-06T10:12:00Z"
    }
  ]
}
`)
	if fingerprint(base) != fingerprint(restamped) {
		t.Error("a payload restamped with a later capture instant compares as changed; " +
			"every predicate would then be vacuous")
	}

	nested := []byte(`{
  "kind": "node_metadata",
  "captured_at": "2026-08-06T10:12:00Z",
  "nodes": [
    {
      "name": "node-1",
      "captured_at": "2026-08-06T10:14:00Z"
    }
  ]
}
`)
	if fingerprint(base) == fingerprint(nested) {
		t.Error("a field named captured_at below the top level was elided too; the elision " +
			"is the payload's own capture instant, not every field that shares its name")
	}

	changed := []byte(`{
  "kind": "node_metadata",
  "captured_at": "2026-08-06T10:12:00Z",
  "nodes": [
    {
      "name": "node-2",
      "captured_at": "2026-08-06T10:12:00Z"
    }
  ]
}
`)
	if fingerprint(base) == fingerprint(changed) {
		t.Error("a changed node name compares as unchanged")
	}
}

// The gate remembers what the spool holds, and forgets on the same cutoff. A
// key that outlives its entry is written whole again, which is the safe way
// round; a map that never forgets is the other one.
func TestTheGateForgetsWhatTheSweepDropped(t *testing.T) {
	s, dir := newTestSpool(t)
	clk := driveClock(s, capturedAt)

	if err := s.WriteNodeMetadata(capturedAt, fixedNodeMetadata()); err != nil {
		t.Fatal(err)
	}
	clk.advance(2 * s.maxAge)
	if err := s.Sweep(clk.at); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteNodeMetadata(clk.at, fixedNodeMetadata()); err != nil {
		t.Fatal(err)
	}
	if got := capturedAtOf(t, dir, "node-metadata.json"); !got.Equal(clk.at) {
		t.Errorf("node metadata was captured at %s, want %s; after the sweep dropped the file "+
			"the gate had no version to compare against", got, clk.at)
	}
}

func mustLookup(t *testing.T, kind string) PayloadKind {
	t.Helper()
	entry, ok := Lookup(kind)
	if !ok {
		t.Fatalf("kind %q has no registry row", kind)
	}
	return entry
}
