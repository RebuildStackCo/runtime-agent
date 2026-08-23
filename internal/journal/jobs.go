package journal

import (
	"sort"
	"sync"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

// JobRunRecord is one finished Job run, in the window where it finished.
//
// Like a disruption and unlike a restart, a run is timestamped by the cluster,
// so the record carries the instants themselves and the window is only a
// delivery boundary (ADR 0021, ADR 0029). The pair of instants is the point of
// the payload: it is the denominator a usage rollup cannot supply for a
// workload that ran for ninety seconds of an hour-long window.
type JobRunRecord struct {
	Namespace string                `json:"namespace"`
	Workload  collector.WorkloadRef `json:"workload"`
	// Name is the run's own object name, which for a CronJob is generated per
	// run and is what tells two runs of one schedule apart.
	Name string `json:"name"`
	// StartedAt is zero for a Job that failed before the controller recorded a
	// start. A zero instant is a real state — declared and never started — and
	// is omitted rather than rendered as the epoch.
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at"`
	// Result is "succeeded" or "failed", from the Job's terminal condition.
	Result string `json:"result"`
	// FailReason is the terminal condition's reason — `BackoffLimitExceeded`
	// and `DeadlineExceeded` are different capacity facts. Kubernetes'
	// vocabulary, never the free-text message beside it (ADR 0020 §6).
	FailReason string `json:"fail_reason,omitempty"`
	// Succeeded and Failed count the run's own pods. A run that succeeded with
	// Failed > 0 retried, and the retries consumed resources nothing else
	// reports.
	Succeeded int32 `json:"succeeded"`
	Failed    int32 `json:"failed"`
	// The declared shape. Without it "3 succeeded" cannot be read as a complete
	// run or a partial one. Nil means the field was unset and Kubernetes'
	// default applies — a fact, distinct from an explicit value.
	Parallelism   *int32    `json:"parallelism,omitempty"`
	Completions   *int32    `json:"completions,omitempty"`
	BackoffLimit  *int32    `json:"backoff_limit,omitempty"`
	WindowStart   time.Time `json:"window_start"`
	WindowSeconds int64     `json:"window_seconds"`
}

type jobRunKey struct {
	namespace string
	name      string
	start     int64
}

// JobRuns collects finished Job runs into wall-clock-aligned windows. It is
// safe for concurrent use: observations arrive on the informer goroutine while
// the flush goroutine reads snapshots.
//
// Like Disruptions it accumulates records rather than counters — a run finishes
// once and there is nothing to add up. The window bounds the payload's file
// count, which matters here because a fleet of CronJobs can finish hundreds of
// runs an hour and one file per run would put the spool's file count under the
// cluster's control (ADR 0021's reasoning, same shape).
type JobRuns struct {
	mu           sync.Mutex
	windowLength time.Duration
	open         map[jobRunKey]JobRunRecord
}

// NewJobRuns returns an accumulator over windows of the given length, which
// must be positive.
func NewJobRuns(windowLength time.Duration) *JobRuns {
	if windowLength <= 0 {
		panic("journal: window length must be positive")
	}
	return &JobRuns{windowLength: windowLength, open: map[jobRunKey]JobRunRecord{}}
}

// Observe files one run in the window holding the instant it finished.
//
// Keying by (namespace, name, window) makes this idempotent: the same run
// reported twice — after an agent restart, while the object still exists —
// rewrites its own record rather than appearing twice. That is what keeps the
// collector's in-memory dedup loss-harmless.
func (j *JobRuns) Observe(run collector.JobRun) {
	if run.FinishedAt.IsZero() {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	start := run.FinishedAt.UTC().Truncate(j.windowLength)
	j.open[jobRunKey{namespace: run.Namespace, name: run.Name, start: start.UnixNano()}] = JobRunRecord{
		Namespace:     run.Namespace,
		Workload:      run.Workload,
		Name:          run.Name,
		StartedAt:     run.StartedAt.UTC(),
		FinishedAt:    run.FinishedAt.UTC(),
		Result:        run.Result,
		FailReason:    run.FailReason,
		Succeeded:     run.Succeeded,
		Failed:        run.Failed,
		Parallelism:   run.Parallelism,
		Completions:   run.Completions,
		BackoffLimit:  run.BackoffLimit,
		WindowStart:   start,
		WindowSeconds: int64(j.windowLength / time.Second),
	}
}

// Snapshots returns every open record, sorted by window then namespace then
// name so the payload bytes are deterministic (the golden contract).
func (j *JobRuns) Snapshots() []JobRunRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]JobRunRecord, 0, len(j.open))
	for _, rec := range j.open {
		out = append(out, rec)
	}
	sortJobRuns(out)
	return out
}

// CloseBefore removes and returns every record whose window ended at or before
// now. Removal bounds memory; the records are returned so the caller writes them
// one last time, with the same loss bound the usage accumulator accepts
// (ADR 0007).
func (j *JobRuns) CloseBefore(now time.Time) []JobRunRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []JobRunRecord
	for key, rec := range j.open {
		if rec.WindowStart.Add(j.windowLength).After(now) {
			continue
		}
		out = append(out, rec)
		delete(j.open, key)
	}
	sortJobRuns(out)
	return out
}

func sortJobRuns(records []JobRunRecord) {
	sort.Slice(records, func(i, k int) bool {
		a, b := records[i], records[k]
		if !a.WindowStart.Equal(b.WindowStart) {
			return a.WindowStart.Before(b.WindowStart)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
}
