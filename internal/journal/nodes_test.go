package journal

import (
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

var nodeWindow = time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

func joined(name string, at time.Time) collector.NodeLifecycle {
	return collector.NodeLifecycle{
		Node:   collector.NodeInfo{Name: name, CapacityCPUMilli: 2000, InstanceType: "m6i.large"},
		Joined: true,
		At:     at,
	}
}

func left(at time.Time) collector.NodeLifecycle {
	return collector.NodeLifecycle{
		Node:     collector.NodeInfo{Name: "node-1", CapacityCPUMilli: 8000, InstanceType: "g5.2xlarge"},
		At:       at,
		Observed: true,
	}
}

// The two events of one node are two records, not one overwriting the other:
// a spot node that arrives and is reclaimed within the hour is the case the
// payload exists for.
func TestArrivalAndDepartureOfOneNodeAreBothKept(t *testing.T) {
	e := NewNodeEvents(time.Hour)
	e.Observe(joined("node-1", nodeWindow.Add(5*time.Minute)))
	e.Observe(left(nodeWindow.Add(50 * time.Minute)))

	records := e.Snapshots()
	if len(records) != 2 {
		t.Fatalf("kept %d records, want 2: %+v", len(records), records)
	}
	if records[0].Event != "joined" || records[1].Event != "left" {
		t.Errorf("events = %q, %q; want joined then left", records[0].Event, records[1].Event)
	}
}

// Idempotent under the same key, which is what keeps the collector's in-memory
// dedup loss-harmless — the property Disruptions.Observe rests on.
func TestTheSameEventReportedTwiceIsOneRecord(t *testing.T) {
	e := NewNodeEvents(time.Hour)
	e.Observe(left(nodeWindow.Add(20 * time.Minute)))
	e.Observe(left(nodeWindow.Add(25 * time.Minute)))

	if records := e.Snapshots(); len(records) != 1 {
		t.Fatalf("kept %d records for one node leaving, want 1", len(records))
	}
}

// The size travels with the event because nothing else will hold it once the
// node object is gone (ADR 0064 §3).
func TestTheDepartureRecordCarriesTheNodesSize(t *testing.T) {
	e := NewNodeEvents(time.Hour)
	e.Observe(left(nodeWindow.Add(20 * time.Minute)))

	rec := e.Snapshots()[0]
	if rec.CapacityCPUMilli != 8000 || rec.InstanceType != "g5.2xlarge" {
		t.Errorf("record = %+v, want the departed node's own size and kind", rec)
	}
	if !rec.AtObserved {
		t.Error("a departure's instant is the agent's observation and must say so")
	}
}

// Events land in the window holding their instant, and a closed window is
// returned once and then forgotten — the bound every journal here accepts.
func TestClosedWindowsAreReturnedOnceAndForgotten(t *testing.T) {
	e := NewNodeEvents(time.Hour)
	e.Observe(joined("node-1", nodeWindow.Add(10*time.Minute)))
	e.Observe(joined("node-2", nodeWindow.Add(90*time.Minute)))

	closed := e.CloseBefore(nodeWindow.Add(90 * time.Minute))
	if len(closed) != 1 || closed[0].Node != "node-1" {
		t.Fatalf("closed %+v, want only the first window's record", closed)
	}
	if again := e.CloseBefore(nodeWindow.Add(90 * time.Minute)); len(again) != 0 {
		t.Errorf("closed window returned twice: %+v", again)
	}
	if open := e.Snapshots(); len(open) != 1 || open[0].Node != "node-2" {
		t.Errorf("open records = %+v, want the still-open window's", open)
	}
}

// An event with nothing to place it is not a record. Both guards matter: the
// zero instant would truncate to the epoch's window and sit in the map forever.
func TestAnUnplaceableEventIsNotRecorded(t *testing.T) {
	e := NewNodeEvents(time.Hour)
	e.Observe(collector.NodeLifecycle{Node: collector.NodeInfo{Name: "node-1"}})
	e.Observe(collector.NodeLifecycle{At: nodeWindow, Joined: true})

	if records := e.Snapshots(); len(records) != 0 {
		t.Errorf("kept %+v, want nothing", records)
	}
}
