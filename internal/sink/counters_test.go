package sink

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

// TestEveryRegisteredKindHasASeriesBeforeItShips. A kind that has never been
// written must read as a zero rather than as an absence: "no usage_window has
// been written in an hour" is the reading an operator alerts on, and it needs
// the series to exist (ADR 0070 §3).
func TestEveryRegisteredKindHasASeriesBeforeItShips(t *testing.T) {
	s, _ := newTestSpool(t)
	c := s.Counters()
	for _, kind := range Registry() {
		if _, ok := c.Written[kind.Kind]; !ok {
			t.Errorf("kind %q has no written counter", kind.Kind)
		}
		if _, ok := c.WriteFailures[kind.Kind]; !ok {
			t.Errorf("kind %q has no failure counter", kind.Kind)
		}
	}
	if len(c.Written) != len(Registry()) {
		t.Errorf("the spool counts %d kinds and the registry holds %d; the registry is the one list (ADR 0022)",
			len(c.Written), len(Registry()))
	}
	for _, reason := range EvictionReasons {
		if _, ok := c.Evicted[reason]; !ok {
			t.Errorf("eviction reason %q has no counter", reason)
		}
	}
}

// TestAKindWithNoRegistryRowIsRefused makes the registry load-bearing at run
// time, not only in the golden tests: a payload kind added without a row does
// not reach the spool at all.
func TestAKindWithNoRegistryRowIsRefused(t *testing.T) {
	s, dir := newTestSpool(t)
	err := s.write("usage_forecast", "usage-forecast.json", struct{}{})
	if err == nil {
		t.Fatal("a kind with no registry row was written")
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("the refused write left %d entries behind", len(entries))
	}
	if _, ok := s.Counters().Written["usage_forecast"]; ok {
		t.Error("the refused kind opened a counter of its own")
	}
}

// TestAWrittenPayloadIsCountedUnderTheKindInItsBytes: the label and the
// discriminator are the same string, so the two cannot disagree.
func TestAWrittenPayloadIsCountedUnderTheKindInItsBytes(t *testing.T) {
	s, _ := newTestSpool(t)
	if err := s.WriteCollectionCoverage(time.Now(), time.Now(), AgentInfo{}, nil,
		model.Coverage{}, model.PlacementDrops{}, model.NodeDrops{},
		nil, nil, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := s.Counters().Written["collection_coverage"]; got != 1 {
		t.Errorf("collection_coverage counted %d times, want 1", got)
	}
	for kind, n := range s.Counters().Written {
		if kind != "collection_coverage" && n != 0 {
			t.Errorf("%s counted %d times; only one payload was written", kind, n)
		}
	}
}

// TestASweepCountsWhatItRemovedAndWhatItLeft. Nothing kept these numbers
// before: the spool enforced its bounds silently, so a cluster whose payloads
// were being evicted looked identical to one whose payloads were being kept
// (ADR 0042, ADR 0070 §5).
func TestASweepCountsWhatItRemovedAndWhatItLeft(t *testing.T) {
	s, dir := newTestSpool(t)
	now := time.Now()

	// Two payloads past the age cutoff, one orphaned temp file, one current.
	old := now.Add(-2 * DefaultMaxAge)
	for _, f := range []struct {
		name string
		mod  time.Time
	}{
		{"expired-a.json", old},
		{"expired-b.json", old},
		{"orphan.json.tmp", now.Add(-2 * time.Minute)},
		{"current.json", now},
	} {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, f.mod, f.mod); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Sweep(now); err != nil {
		t.Fatal(err)
	}
	c := s.Counters()
	if c.Evicted[EvictedAge] != 2 {
		t.Errorf("age evictions: %d, want 2", c.Evicted[EvictedAge])
	}
	if c.Evicted[EvictedOrphanTemp] != 1 {
		t.Errorf("orphan temp files: %d, want 1", c.Evicted[EvictedOrphanTemp])
	}
	if c.Files != 1 {
		t.Errorf("files left: %d, want 1", c.Files)
	}
	if c.Bytes != 3 {
		t.Errorf("bytes left: %d, want 3", c.Bytes)
	}
}

// TestTheFileCeilingSaysWhichBoundEvicted: bytes and files answer different
// questions — more payload than the volume holds, against more payloads than
// the count allows — so the reason distinguishes them (ADR 0042 §2).
func TestTheFileCeilingSaysWhichBoundEvicted(t *testing.T) {
	s, dir := newTestSpool(t)
	s.maxFiles = 2
	now := time.Now()
	for i, name := range []string{"a.json", "b.json", "c.json", "d.json"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Sweep(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	c := s.Counters()
	if c.Evicted[EvictedFiles] != 2 {
		t.Errorf("file-ceiling evictions: %d, want 2", c.Evicted[EvictedFiles])
	}
	if c.Evicted[EvictedBytes] != 0 {
		t.Errorf("byte-ceiling evictions: %d, want 0", c.Evicted[EvictedBytes])
	}
	if c.Files != 2 {
		t.Errorf("files left: %d, want 2", c.Files)
	}
}
