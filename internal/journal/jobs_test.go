package journal

import (
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

var jobWindow = time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

func run(name string, finishedAt time.Time) collector.JobRun {
	return collector.JobRun{
		Namespace:  "analytics",
		Workload:   collector.WorkloadRef{Kind: "CronJob", Name: "rollup"},
		Name:       name,
		StartedAt:  finishedAt.Add(-time.Minute),
		FinishedAt: finishedAt,
		Result:     collector.JobSucceeded,
		Succeeded:  1,
	}
}

// A run files itself in the window holding the instant it finished, not the
// window that happens to be open when the agent observes it. That is what lets
// a run that finished before startup be reported at all (ADR 0029 §3).
func TestRunFilesItselfByWhenItFinished(t *testing.T) {
	j := NewJobRuns(time.Hour)
	j.Observe(run("rollup-1", jobWindow.Add(20*time.Minute)))
	j.Observe(run("rollup-2", jobWindow.Add(-3*time.Hour)))

	got := j.Snapshots()
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if !got[0].WindowStart.Equal(jobWindow.Add(-3 * time.Hour)) {
		t.Errorf("the older run landed in window %s, want its own", got[0].WindowStart)
	}
	if !got[1].WindowStart.Equal(jobWindow) {
		t.Errorf("the recent run landed in window %s, want %s", got[1].WindowStart, jobWindow)
	}
}

// Re-observing a run rewrites its record rather than adding one. This is what
// makes the collector's in-memory dedup loss-harmless: after a restart the same
// Job object is reported again, with the same instants, into the same window.
func TestReobservingARunRewritesIt(t *testing.T) {
	j := NewJobRuns(time.Hour)
	first := run("rollup-1", jobWindow.Add(20*time.Minute))
	j.Observe(first)

	grown := first
	grown.Failed = 3
	j.Observe(grown)

	got := j.Snapshots()
	if len(got) != 1 {
		t.Fatalf("got %d records after re-observing one run, want 1", len(got))
	}
	if got[0].Failed != 3 {
		t.Errorf("surviving record has failed=%d, want 3 (newest rewrites)", got[0].Failed)
	}
}

// A run with no finish instant is not a run. Filing it would put it in the epoch
// window, which is a fact about nothing.
func TestUnfinishedRunIsNotFiled(t *testing.T) {
	j := NewJobRuns(time.Hour)
	unfinished := run("rollup-1", time.Time{})
	j.Observe(unfinished)
	if got := j.Snapshots(); len(got) != 0 {
		t.Fatalf("filed %d records for a run that never finished, want 0", len(got))
	}
}

// CloseBefore hands back ended windows and forgets them, so memory is bounded by
// the open window rather than by how many runs the cluster has ever finished.
func TestJobRunsCloseBeforeReturnsAndForgetsEndedWindows(t *testing.T) {
	j := NewJobRuns(time.Hour)
	j.Observe(run("old", jobWindow.Add(10*time.Minute)))
	j.Observe(run("current", jobWindow.Add(90*time.Minute)))

	closed := j.CloseBefore(jobWindow.Add(2 * time.Hour))
	if len(closed) != 2 {
		t.Fatalf("closed %d records, want 2 (both windows ended)", len(closed))
	}
	if got := j.Snapshots(); len(got) != 0 {
		t.Errorf("accumulator still holds %d records after closing every window", len(got))
	}

	j.Observe(run("current", jobWindow.Add(90*time.Minute)))
	if closed := j.CloseBefore(jobWindow.Add(90 * time.Minute)); len(closed) != 0 {
		t.Errorf("closed %d records of a window that has not ended", len(closed))
	}
}
