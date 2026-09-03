package journal

import (
	"sort"
	"sync"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

// DisruptionRecord is one pod the cluster removed, in the window where it
// happened. Unlike a restart, a disruption is timestamped by Kubernetes, so the
// record carries the moment itself rather than only the window it fell in.
type DisruptionRecord struct {
	Namespace string            `json:"namespace"`
	Pod       string            `json:"pod"`
	Workload  model.WorkloadRef `json:"workload"`
	// Node is where the pod was running. For a node-pressure eviction it names
	// the node that was under pressure; joined against node metadata it turns
	// "a pod was evicted" into "this instance type ran out of memory".
	Node          string    `json:"node,omitempty"`
	Reason        string    `json:"reason"`
	DisruptedAt   time.Time `json:"disrupted_at"`
	WindowStart   time.Time `json:"window_start"`
	WindowSeconds int64     `json:"window_seconds"`
}

type disruptionKey struct {
	namespace string
	pod       string
	start     int64
}

// Disruptions collects disrupted pods into wall-clock-aligned windows. It is
// safe for concurrent use: observations arrive on the informer goroutine while
// the flush goroutine reads snapshots.
//
// Unlike Restarts it accumulates records rather than counters: a disruption is
// terminal and there is nothing to add up. The window bounds the payload's file
// count and lines the events up with the usage and restart windows of the same
// hour, not because they aggregate.
type Disruptions struct {
	mu           sync.Mutex
	windowLength time.Duration
	open         map[disruptionKey]DisruptionRecord
}

// NewDisruptions returns an accumulator over windows of the given length, which
// must be positive.
func NewDisruptions(windowLength time.Duration) *Disruptions {
	if windowLength <= 0 {
		panic("journal: window length must be positive")
	}
	return &Disruptions{windowLength: windowLength, open: map[disruptionKey]DisruptionRecord{}}
}

// Observe files one disruption in the window holding the instant Kubernetes
// recorded for it.
//
// Keying by (namespace, pod, window) makes this idempotent: the same pod
// reported twice — after an agent restart, say — rewrites its own record rather
// than appearing twice. That is what keeps the collector's in-memory dedup
// loss-harmless.
func (d *Disruptions) Observe(e model.PodDisruption) {
	if e.DisruptedAt.IsZero() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	start := e.DisruptedAt.UTC().Truncate(d.windowLength)
	d.open[disruptionKey{namespace: e.Namespace, pod: e.Pod, start: start.UnixNano()}] = DisruptionRecord{
		Namespace:     e.Namespace,
		Pod:           e.Pod,
		Workload:      e.Workload,
		Node:          e.Node,
		Reason:        e.Reason,
		DisruptedAt:   e.DisruptedAt.UTC(),
		WindowStart:   start,
		WindowSeconds: int64(d.windowLength / time.Second),
	}
}

// Snapshots returns every open record, sorted by window and then by pod so the
// payload bytes are deterministic (the golden contract).
func (d *Disruptions) Snapshots() []DisruptionRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]DisruptionRecord, 0, len(d.open))
	for _, rec := range d.open {
		out = append(out, rec)
	}
	sortDisruptions(out)
	return out
}

// CloseBefore removes and returns every record whose window ended at or before
// now. Removal bounds memory; the records are returned so the caller writes
// them one last time, with the same loss bound the usage accumulator accepts
// (ADR 0007).
func (d *Disruptions) CloseBefore(now time.Time) []DisruptionRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []DisruptionRecord
	for key, rec := range d.open {
		if rec.WindowStart.Add(d.windowLength).After(now) {
			continue
		}
		out = append(out, rec)
		delete(d.open, key)
	}
	sortDisruptions(out)
	return out
}

func sortDisruptions(records []DisruptionRecord) {
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if !a.WindowStart.Equal(b.WindowStart) {
			return a.WindowStart.Before(b.WindowStart)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Pod < b.Pod
	})
}
