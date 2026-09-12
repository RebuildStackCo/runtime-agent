package journal

import (
	"sort"
	"sync"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

// The goroutine series is the one journal whose facts the agent reads from a
// workload rather than from an object's status, and it is a journal anyway: the
// shape is the question. A goroutine count answers nothing at an instant and
// everything over time, so what is collected is a series within a window, which
// is what the four journals beside it already are (ADR 0080 §4).

// maxSamplesPerWindow bounds one record. The poll interval and the window are
// both constants, so a full window holds one sample a minute; the cap is twice
// that, and exists so a future interval cannot grow the payload without anyone
// noticing. Samples past it are dropped and counted, never silently lost.
const maxSamplesPerWindow = 120

// GoroutineSample is one reading of one process's goroutine count.
type GoroutineSample struct {
	At         time.Time `json:"at"`
	Goroutines int64     `json:"goroutines"`
}

// GoroutineRecord is one process's goroutine series within one window.
//
// Keyed by pod, like the restart journal and for the same kind of reason: the
// count belongs to a process, and a per-workload total would average away the
// one replica that is leaking.
type GoroutineRecord struct {
	Key
	Workload      model.WorkloadRef `json:"workload"`
	ImageDigest   string            `json:"image_digest"`
	WindowStart   time.Time         `json:"window_start"`
	WindowSeconds int64             `json:"window_seconds"`
	// Samples is the readings taken within the window, oldest first.
	Samples []GoroutineSample `json:"samples"`
	// SamplesDropped is readings the cap refused. Non-zero means the series is
	// thinner than the interval implies, which a reader has to know before
	// calling a gap in it a gap in the process.
	SamplesDropped int64 `json:"samples_dropped,omitempty"`
}

// Goroutines accumulates goroutine counts into wall-clock-aligned windows,
// the same windows the usage rollups use so the two are read side by side.
type Goroutines struct {
	mu           sync.Mutex
	windowLength time.Duration
	open         map[openKey]*GoroutineRecord
}

// NewGoroutines returns an accumulator over windows of the given length, which
// must be positive.
func NewGoroutines(windowLength time.Duration) *Goroutines {
	if windowLength <= 0 {
		panic("journal: window length must be positive")
	}
	return &Goroutines{windowLength: windowLength, open: map[openKey]*GoroutineRecord{}}
}

// Observe folds one reading into the window holding the instant it was read.
func (g *Goroutines) Observe(c model.GoroutineCount) {
	if c.Goroutines <= 0 {
		return // a live Go process runs at least one goroutine; this is a misread
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	start := c.ObservedAt.UTC().Truncate(g.windowLength)
	key := openKey{
		Key:   Key{Namespace: c.Namespace, Pod: c.Pod, Container: c.Container},
		start: start.UnixNano(),
	}
	rec := g.open[key]
	if rec == nil {
		rec = &GoroutineRecord{
			Key:           key.Key,
			Workload:      c.Workload,
			ImageDigest:   c.ImageDigest,
			WindowStart:   start,
			WindowSeconds: int64(g.windowLength / time.Second),
		}
		g.open[key] = rec
	}
	if len(rec.Samples) >= maxSamplesPerWindow {
		rec.SamplesDropped++
		return
	}
	rec.Samples = append(rec.Samples, GoroutineSample{
		At:         c.ObservedAt.UTC(),
		Goroutines: c.Goroutines,
	})
}

// Snapshots returns deep copies of every open record, sorted by key and window
// so the payload bytes are deterministic (the golden contract). Callers may
// retain the result.
func (g *Goroutines) Snapshots() []GoroutineRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]GoroutineRecord, 0, len(g.open))
	for _, rec := range g.open {
		out = append(out, rec.clone())
	}
	sortGoroutineRecords(out)
	return out
}

// Resume seeds open windows from records a previous process of this agent wrote
// to the spool, so a restart continues the window rather than writing a subset
// of it back over itself (ADR 0077). A key already present is left alone.
//
// Nothing is double counted: a sample is a reading at an instant, not an
// advance, so the same instant read twice is the same value and a resumed
// window holds readings this process could no longer take.
func (g *Goroutines) Resume(records []GoroutineRecord) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	var seeded int
	for _, rec := range records {
		key := openKey{Key: rec.Key, start: rec.WindowStart.UnixNano()}
		if _, ok := g.open[key]; ok {
			continue
		}
		resumed := rec.clone()
		if len(resumed.Samples) > maxSamplesPerWindow {
			resumed.SamplesDropped += int64(len(resumed.Samples) - maxSamplesPerWindow)
			resumed.Samples = resumed.Samples[:maxSamplesPerWindow]
		}
		g.open[key] = &resumed
		seeded++
	}
	return seeded
}

// CloseBefore removes and returns every record whose window ended at or before
// now. Removal is what bounds memory, exactly as it is for the journals beside
// this one.
func (g *Goroutines) CloseBefore(now time.Time) []GoroutineRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []GoroutineRecord
	for key, rec := range g.open {
		if rec.WindowStart.Add(g.windowLength).After(now) {
			continue
		}
		out = append(out, rec.clone())
		delete(g.open, key)
	}
	sortGoroutineRecords(out)
	return out
}

func (g *GoroutineRecord) clone() GoroutineRecord {
	out := *g
	out.Samples = make([]GoroutineSample, len(g.Samples))
	copy(out.Samples, g.Samples)
	// Sorted here rather than trusted from insertion: the payload bytes are a
	// golden contract, and they must not depend on the order readings arrived.
	sort.Slice(out.Samples, func(i, j int) bool {
		return out.Samples[i].At.Before(out.Samples[j].At)
	})
	return out
}

func sortGoroutineRecords(records []GoroutineRecord) {
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if !a.WindowStart.Equal(b.WindowStart) {
			return a.WindowStart.Before(b.WindowStart)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Pod != b.Pod {
			return a.Pod < b.Pod
		}
		return a.Container < b.Container
	})
}
