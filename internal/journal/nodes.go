package journal

import (
	"sort"
	"sync"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

// NodeEventRecord is one node arriving in or leaving the cluster, in the window
// where it happened (ADR 0064 §3).
//
// It repeats the node's size and kind rather than pointing at `node_metadata`,
// and that repetition is the payload's reason to exist: the snapshot holds only
// live nodes, so by the time anything reads a departure the row it would join
// against is gone. Without the size here, "twelve nodes left this hour" cannot
// become "and they were 96 cores of spot capacity".
type NodeEventRecord struct {
	Node string `json:"node"`
	// Event is "joined" or "left".
	Event string `json:"event"`
	// At is the node object's creation timestamp for an arrival, and the instant
	// the agent noticed for a departure — a deleted object carries no deletion
	// time to read. AtObserved marks the second case rather than letting one
	// field silently mix two provenances.
	At         time.Time `json:"at"`
	AtObserved bool      `json:"at_observed,omitempty"`

	InstanceType string `json:"instance_type,omitempty"`
	CapacityType string `json:"capacity_type,omitempty"`
	Zone         string `json:"zone,omitempty"`
	// Capacity, not allocatable: what left the cluster is a machine, and what it
	// reserved for its own kubelet is not the fleet's loss.
	CapacityCPUMilli    int64 `json:"capacity_cpu_milli,omitempty"`
	CapacityMemoryBytes int64 `json:"capacity_memory_bytes,omitempty"`
	// Devices travel for the same reason as the size above, and matter more: an
	// accelerator node is the expensive one, and its coming and going is the
	// question a fleet with any accelerators at all will be asked first.
	Devices []collector.NodeDevice `json:"devices,omitempty"`

	WindowStart   time.Time `json:"window_start"`
	WindowSeconds int64     `json:"window_seconds"`
}

const (
	eventJoined = "joined"
	eventLeft   = "left"
)

type nodeEventKey struct {
	node  string
	event string
	start int64
}

// NodeEvents collects node arrivals and departures into wall-clock-aligned
// windows. It is safe for concurrent use: observations arrive on the informer
// goroutine while the flush goroutine reads snapshots.
//
// Like Disruptions it accumulates records rather than counters: an arrival is
// terminal and there is nothing to add up. Keying by (node, event, window)
// collapses a node that cycles twice within one window to one record per event
// type — the collapse Disruptions accepts, on the bound ADR 0064 §3 sets.
type NodeEvents struct {
	mu           sync.Mutex
	windowLength time.Duration
	open         map[nodeEventKey]NodeEventRecord
}

// NewNodeEvents returns an accumulator over windows of the given length, which
// must be positive.
func NewNodeEvents(windowLength time.Duration) *NodeEvents {
	if windowLength <= 0 {
		panic("journal: window length must be positive")
	}
	return &NodeEvents{windowLength: windowLength, open: map[nodeEventKey]NodeEventRecord{}}
}

// Observe files one lifecycle event in the window holding its instant.
func (e *NodeEvents) Observe(ev collector.NodeLifecycle) {
	if ev.At.IsZero() || ev.Node.Name == "" {
		return
	}
	event := eventLeft
	if ev.Joined {
		event = eventJoined
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	start := ev.At.UTC().Truncate(e.windowLength)
	e.open[nodeEventKey{node: ev.Node.Name, event: event, start: start.UnixNano()}] = NodeEventRecord{
		Node:                ev.Node.Name,
		Event:               event,
		At:                  ev.At.UTC(),
		AtObserved:          ev.Observed,
		InstanceType:        ev.Node.InstanceType,
		CapacityType:        ev.Node.CapacityType,
		Zone:                ev.Node.Zone,
		CapacityCPUMilli:    ev.Node.CapacityCPUMilli,
		CapacityMemoryBytes: ev.Node.CapacityMemoryBytes,
		Devices:             ev.Node.Devices,
		WindowStart:         start,
		WindowSeconds:       int64(e.windowLength / time.Second),
	}
}

// Snapshots returns every open record, sorted so the payload bytes are
// deterministic (the golden contract).
func (e *NodeEvents) Snapshots() []NodeEventRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]NodeEventRecord, 0, len(e.open))
	for _, rec := range e.open {
		out = append(out, rec)
	}
	sortNodeEvents(out)
	return out
}

// CloseBefore removes and returns every record whose window ended at or before
// now, with the same bound and the same reasoning as Disruptions.CloseBefore.
func (e *NodeEvents) CloseBefore(now time.Time) []NodeEventRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []NodeEventRecord
	for key, rec := range e.open {
		if rec.WindowStart.Add(e.windowLength).After(now) {
			continue
		}
		out = append(out, rec)
		delete(e.open, key)
	}
	sortNodeEvents(out)
	return out
}

func sortNodeEvents(records []NodeEventRecord) {
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if !a.WindowStart.Equal(b.WindowStart) {
			return a.WindowStart.Before(b.WindowStart)
		}
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		return a.Event < b.Event
	})
}
