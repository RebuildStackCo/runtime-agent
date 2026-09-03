package journal

import (
	"testing"
	"time"

	"k8s.io/utils/ptr"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

var windowStart = time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)

func restart(at time.Time, count int64, reason string) model.ContainerRestart {
	return model.ContainerRestart{
		Namespace:  "shop",
		Pod:        "web-1",
		Container:  "app",
		Workload:   model.WorkloadRef{Kind: "Deployment", Name: "web"},
		ObservedAt: at,
		Restarts:   count,
		Reason:     reason,
		ExitCode:   ptr.To(int32(1)),
	}
}

func TestObserveAccumulatesWithinOneWindow(t *testing.T) {
	r := NewRestarts(time.Hour)
	r.Observe(restart(windowStart.Add(5*time.Minute), 1, "Error"))
	r.Observe(restart(windowStart.Add(20*time.Minute), 1, "OOMKilled"))
	r.Observe(restart(windowStart.Add(50*time.Minute), 1, "Error"))

	got := r.Snapshots()
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1 (one container, one window)", len(got))
	}
	rec := got[0]
	if rec.Restarts != 3 {
		t.Errorf("restarts = %d, want 3", rec.Restarts)
	}
	if rec.Reasons["Error"] != 2 || rec.Reasons["OOMKilled"] != 1 {
		t.Errorf("reasons = %v, want Error:2 OOMKilled:1", rec.Reasons)
	}
	if rec.ReasonsUnobserved != 0 {
		t.Errorf("unobserved = %d, want 0 — every restart's reason was seen", rec.ReasonsUnobserved)
	}
	if !rec.WindowStart.Equal(windowStart) || rec.WindowSeconds != 3600 {
		t.Errorf("window = %s/%ds, want %s/3600", rec.WindowStart, rec.WindowSeconds, windowStart)
	}
	if rec.Workload != (model.WorkloadRef{Kind: "Deployment", Name: "web"}) {
		t.Errorf("workload = %+v, want Deployment/web", rec.Workload)
	}
}

// The counter is exact and the reason is a sample of it. A container that
// restarted five times between two status updates shows one reason and four
// restarts whose reason nobody saw — stated as a number, not implied by a
// breakdown that sums to less than the total.
func TestUnobservedReasonsCarryTheDifference(t *testing.T) {
	r := NewRestarts(time.Hour)
	r.Observe(restart(windowStart.Add(time.Minute), 5, "Error"))

	rec := r.Snapshots()[0]
	if rec.Restarts != 5 {
		t.Errorf("restarts = %d, want 5", rec.Restarts)
	}
	if rec.Reasons["Error"] != 1 {
		t.Errorf("reasons = %v, want Error:1 — only one termination was visible", rec.Reasons)
	}
	if rec.ReasonsUnobserved != 4 {
		t.Errorf("unobserved = %d, want 4", rec.ReasonsUnobserved)
	}
	if sum(rec.Reasons)+rec.ReasonsUnobserved != rec.Restarts {
		t.Errorf("reasons %v plus %d unobserved must equal %d restarts",
			rec.Reasons, rec.ReasonsUnobserved, rec.Restarts)
	}
}

// A restart whose status carried no terminated state at all is counted with
// nothing observed about it.
func TestRestartWithNoReasonIsFullyUnobserved(t *testing.T) {
	r := NewRestarts(time.Hour)
	r.Observe(restart(windowStart.Add(time.Minute), 2, ""))

	rec := r.Snapshots()[0]
	if len(rec.Reasons) != 0 || rec.ReasonsUnobserved != 2 {
		t.Errorf("reasons = %v, unobserved = %d; want no reasons and 2 unobserved",
			rec.Reasons, rec.ReasonsUnobserved)
	}
}

// The reason set belongs to the kubelet and the container runtime, so an
// unfamiliar value is counted rather than dropped — and counted under a key of
// ours, so a CRI cannot set the payload's cardinality.
func TestUnknownReasonIsBucketedNotDropped(t *testing.T) {
	r := NewRestarts(time.Hour)
	r.Observe(restart(windowStart.Add(time.Minute), 1, "SomeFutureRuntimeReason"))

	rec := r.Snapshots()[0]
	if rec.Reasons[UnknownReason] != 1 {
		t.Errorf("reasons = %v, want the unfamiliar reason counted under %q", rec.Reasons, UnknownReason)
	}
	if rec.Restarts != 1 || rec.ReasonsUnobserved != 0 {
		t.Errorf("record = %+v; bucketing must not change the totals", rec)
	}
}

func TestObservationsSplitAcrossWindows(t *testing.T) {
	r := NewRestarts(time.Hour)
	r.Observe(restart(windowStart.Add(30*time.Minute), 1, "Error"))
	r.Observe(restart(windowStart.Add(90*time.Minute), 2, "Error"))

	got := r.Snapshots()
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2 (two windows)", len(got))
	}
	if !got[0].WindowStart.Equal(windowStart) || got[0].Restarts != 1 {
		t.Errorf("first window = %+v, want 1 restart at %s", got[0], windowStart)
	}
	if !got[1].WindowStart.Equal(windowStart.Add(time.Hour)) || got[1].Restarts != 2 {
		t.Errorf("second window = %+v, want 2 restarts at %s", got[1], windowStart.Add(time.Hour))
	}
}

// Two containers of the same pod are two records: which container is in the
// crash loop is the question the pod-scoped key exists to answer.
func TestContainersOfOnePodAreSeparateRecords(t *testing.T) {
	r := NewRestarts(time.Hour)
	first := restart(windowStart.Add(time.Minute), 1, "Error")
	second := first
	second.Container = "sidecar"
	r.Observe(first)
	r.Observe(second)

	got := r.Snapshots()
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2 (one per container)", len(got))
	}
	if got[0].Container != "app" || got[1].Container != "sidecar" {
		t.Errorf("containers = %q, %q; want app, sidecar in sorted order", got[0].Container, got[1].Container)
	}
}

func TestCloseBeforeReturnsAndForgetsEndedWindows(t *testing.T) {
	r := NewRestarts(time.Hour)
	r.Observe(restart(windowStart.Add(30*time.Minute), 1, "Error"))
	r.Observe(restart(windowStart.Add(90*time.Minute), 1, "Error"))

	closed := r.CloseBefore(windowStart.Add(time.Hour))
	if len(closed) != 1 || !closed[0].WindowStart.Equal(windowStart) {
		t.Fatalf("closed = %+v, want only the window ending at %s", closed, windowStart.Add(time.Hour))
	}
	// Dropping closed windows is what bounds memory; the still-open one stays.
	open := r.Snapshots()
	if len(open) != 1 || !open[0].WindowStart.Equal(windowStart.Add(time.Hour)) {
		t.Fatalf("open = %+v, want only the second window", open)
	}
	if again := r.CloseBefore(windowStart.Add(time.Hour)); len(again) != 0 {
		t.Errorf("closing twice returned %+v, want nothing the second time", again)
	}
}

// Snapshots hand out copies: a caller writing into what it got back must not
// reach into the accumulator's own records.
func TestSnapshotsDoNotAliasStoreState(t *testing.T) {
	r := NewRestarts(time.Hour)
	r.Observe(restart(windowStart.Add(time.Minute), 1, "Error"))

	got := r.Snapshots()[0]
	got.Reasons["Error"] = 99
	if again := r.Snapshots()[0]; again.Reasons["Error"] != 1 {
		t.Errorf("Error count = %d after the caller mutated its copy, want 1", again.Reasons["Error"])
	}
}

// A zero or negative advance is not an observation. Guarding here keeps every
// record's arithmetic (reasons plus unobserved equals restarts) true by
// construction.
func TestNonPositiveAdvanceIsIgnored(t *testing.T) {
	r := NewRestarts(time.Hour)
	r.Observe(restart(windowStart.Add(time.Minute), 0, "Error"))
	r.Observe(restart(windowStart.Add(time.Minute), -3, "Error"))

	if got := r.Snapshots(); len(got) != 0 {
		t.Errorf("got %+v, want no records", got)
	}
}

func sum(counts map[string]int64) int64 {
	var total int64
	for _, n := range counts {
		total += n
	}
	return total
}
