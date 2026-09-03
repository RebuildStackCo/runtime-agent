package journal

import (
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

func disruption(at time.Time, pod, reason string) model.PodDisruption {
	return model.PodDisruption{
		Namespace:   "shop",
		Pod:         pod,
		Workload:    model.WorkloadRef{Kind: "Deployment", Name: "web"},
		Node:        "node-1",
		Reason:      reason,
		DisruptedAt: at,
	}
}

func TestObserveFilesDisruptionsByTheirOwnTimestamp(t *testing.T) {
	d := NewDisruptions(time.Hour)
	d.Observe(disruption(windowStart.Add(10*time.Minute), "web-1", "PreemptionByScheduler"))
	d.Observe(disruption(windowStart.Add(70*time.Minute), "web-2", "TerminationByKubelet"))

	got := d.Snapshots()
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if !got[0].WindowStart.Equal(windowStart) || got[0].Pod != "web-1" {
		t.Errorf("first = %+v, want web-1 in the window at %s", got[0], windowStart)
	}
	if !got[1].WindowStart.Equal(windowStart.Add(time.Hour)) || got[1].Pod != "web-2" {
		t.Errorf("second = %+v, want web-2 in the next window", got[1])
	}
	if !got[0].DisruptedAt.Equal(windowStart.Add(10 * time.Minute)) {
		t.Errorf("disrupted_at = %s, want the cluster's own instant", got[0].DisruptedAt)
	}
	if got[0].Node != "node-1" {
		t.Errorf("node = %q, want node-1", got[0].Node)
	}
}

// The same pod reported twice — after an agent restart, say — rewrites its own
// record instead of appearing twice. That is what makes the collector's
// in-memory dedup loss-harmless.
func TestObserveIsIdempotentPerPodAndWindow(t *testing.T) {
	d := NewDisruptions(time.Hour)
	at := windowStart.Add(10 * time.Minute)
	d.Observe(disruption(at, "web-1", "PreemptionByScheduler"))
	d.Observe(disruption(at, "web-1", "PreemptionByScheduler"))

	if got := d.Snapshots(); len(got) != 1 {
		t.Fatalf("got %d records for one disruption reported twice, want 1", len(got))
	}
}

// A disruption with no timestamp cannot be placed in a window, and a window
// chosen for it would be an invention.
func TestObserveIgnoresAnUntimestampedDisruption(t *testing.T) {
	d := NewDisruptions(time.Hour)
	d.Observe(disruption(time.Time{}, "web-1", "PreemptionByScheduler"))

	if got := d.Snapshots(); len(got) != 0 {
		t.Errorf("got %+v, want no records", got)
	}
}

func TestCloseBeforeReturnsAndForgetsEndedDisruptionWindows(t *testing.T) {
	d := NewDisruptions(time.Hour)
	d.Observe(disruption(windowStart.Add(10*time.Minute), "web-1", "PreemptionByScheduler"))
	d.Observe(disruption(windowStart.Add(70*time.Minute), "web-2", "TerminationByKubelet"))

	closed := d.CloseBefore(windowStart.Add(time.Hour))
	if len(closed) != 1 || closed[0].Pod != "web-1" {
		t.Fatalf("closed = %+v, want only web-1's window", closed)
	}
	open := d.Snapshots()
	if len(open) != 1 || open[0].Pod != "web-2" {
		t.Fatalf("open = %+v, want only web-2", open)
	}
	if again := d.CloseBefore(windowStart.Add(time.Hour)); len(again) != 0 {
		t.Errorf("closing twice returned %+v, want nothing the second time", again)
	}
}

// Records of one window come out sorted, so the payload bytes are stable.
func TestDisruptionSnapshotsAreSorted(t *testing.T) {
	d := NewDisruptions(time.Hour)
	at := windowStart.Add(10 * time.Minute)
	d.Observe(disruption(at, "web-9", "PreemptionByScheduler"))
	d.Observe(disruption(at, "web-1", "PreemptionByScheduler"))

	got := d.Snapshots()
	if len(got) != 2 || got[0].Pod != "web-1" || got[1].Pod != "web-9" {
		t.Errorf("order = %q, %q; want web-1 before web-9", got[0].Pod, got[1].Pod)
	}
}
