// Package journal reduces what the cluster's own history says about a workload
// into windowed aggregates. Its facts come from object status — restart
// counters, terminations — which is what makes their provenance `journal`
// rather than `measured` or `structural` (ADR 0012).
//
// The windows are wall-clock aligned and the same length as the usage windows:
// a restart count is read next to the CPU and memory of the same hour, and a
// window that did not line up would make that comparison a judgement call.
package journal

import (
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

// UnknownReason is where a termination reason outside the known set is counted.
// The reason string comes from the kubelet and the container runtime, not from
// user input, but the set is theirs to extend and a payload whose keys are
// chosen by a CRI is a payload with unbounded cardinality. Bucketing keeps the
// key set ours while still counting the event — the count is visible, never
// silently dropped.
const UnknownReason = "other"

// knownReasons is the set of termination reasons carried through under their
// own name. These are the values the kubelet and the CRI implementations
// produce; anything else is counted under UnknownReason.
var knownReasons = map[string]struct{}{
	"OOMKilled":              {},
	"Error":                  {},
	"Completed":              {},
	"ContainerCannotRun":     {},
	"ContainerStatusUnknown": {},
	"DeadlineExceeded":       {},
	"StartError":             {},
	"Evicted":                {},
}

// Key identifies one restart record: one container of one pod. Unlike the
// container-scoped rollup key (ADR 0006) this names the pod, because which
// replica is in a crash loop is the question the record exists to answer, and
// a per-workload total cannot express it.
type Key struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
}

// RestartRecord is one container's restart history within one window.
//
// Restarts is exact and Reasons is a sample of it, so the two do not have to
// agree: ReasonsUnobserved carries the difference rather than letting the
// reason breakdown quietly imply a smaller total (ADR 0013's principle).
type RestartRecord struct {
	Key
	Workload      collector.WorkloadRef `json:"workload"`
	WindowStart   time.Time             `json:"window_start"`
	WindowSeconds int64                 `json:"window_seconds"`
	// Restarts is every restart of this container within the window, counted
	// from the restart counter's advance.
	Restarts int64 `json:"restarts"`
	// Reasons counts the restarts whose termination the agent actually saw,
	// by reason. It sums to Restarts - ReasonsUnobserved.
	Reasons map[string]int64 `json:"reasons"`
	// ReasonsUnobserved is restarts that happened between two status updates,
	// so only the last one's reason was visible. It is not an error state: a
	// container restarting faster than the informer delivers updates is exactly
	// the case worth reporting.
	ReasonsUnobserved int64 `json:"reasons_unobserved"`
	// LastExitCode is the exit code of the most recent termination seen in the
	// window, nil when none carried one.
	LastExitCode *int32 `json:"last_exit_code,omitempty"`
}

type openKey struct {
	Key
	start int64 // window start, unix nanoseconds
}

// Restarts accumulates ContainerRestart observations into wall-clock-aligned
// windows. It is safe for concurrent use: observations arrive on the informer
// goroutine while the flush goroutine reads snapshots.
type Restarts struct {
	mu           sync.Mutex
	windowLength time.Duration
	open         map[openKey]*RestartRecord
}

// NewRestarts returns an accumulator over windows of the given length, which
// must be positive.
func NewRestarts(windowLength time.Duration) *Restarts {
	if windowLength <= 0 {
		panic("journal: window length must be positive")
	}
	return &Restarts{windowLength: windowLength, open: map[openKey]*RestartRecord{}}
}

// Observe folds one observed counter advance into the window holding its
// observation instant.
//
// The whole advance lands in one window rather than being split across the
// windows it may have spanned. A counter delta of usage is spread pro rata
// because consumption accrues continuously; restarts are discrete events whose
// individual times Kubernetes does not record, so pro-rating them would invent
// a distribution rather than approximate one.
func (r *Restarts) Observe(e collector.ContainerRestart) {
	if e.Restarts <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	start := e.ObservedAt.UTC().Truncate(r.windowLength)
	key := openKey{
		Key:   Key{Namespace: e.Namespace, Pod: e.Pod, Container: e.Container},
		start: start.UnixNano(),
	}
	rec := r.open[key]
	if rec == nil {
		rec = &RestartRecord{
			Key:           key.Key,
			Workload:      e.Workload,
			WindowStart:   start,
			WindowSeconds: int64(r.windowLength / time.Second),
			Reasons:       map[string]int64{},
		}
		r.open[key] = rec
	}
	rec.Restarts += e.Restarts
	if e.Reason == "" {
		// No termination state at all: nothing was observed about any of them.
		rec.ReasonsUnobserved += e.Restarts
		return
	}
	rec.Reasons[bucket(e.Reason)]++
	// One termination is visible per observation, so everything the advance
	// counted beyond it went unseen.
	rec.ReasonsUnobserved += e.Restarts - 1
	if e.ExitCode != nil {
		code := *e.ExitCode
		rec.LastExitCode = &code
	}
}

// bucket maps a termination reason to the key it is counted under.
func bucket(reason string) string {
	if _, ok := knownReasons[reason]; ok {
		return reason
	}
	return UnknownReason
}

// Snapshots returns deep copies of every open record, sorted by key and window
// so the payload bytes are deterministic (the golden contract). Callers may
// retain the result.
func (r *Restarts) Snapshots() []RestartRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RestartRecord, 0, len(r.open))
	for _, rec := range r.open {
		out = append(out, rec.clone())
	}
	sortRecords(out)
	return out
}

// CloseBefore removes and returns every record whose window ended at or before
// now — nothing can be added to them any more, since observations are stamped
// with the instant they were seen.
//
// Removal is what bounds memory: without it the map would grow with every
// window the agent lives through. The records are returned rather than dropped
// so the caller writes them one last time; a write that fails loses that
// window, the same bound the usage accumulator already accepts (ADR 0007).
func (r *Restarts) CloseBefore(now time.Time) []RestartRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []RestartRecord
	for key, rec := range r.open {
		if rec.WindowStart.Add(r.windowLength).After(now) {
			continue
		}
		out = append(out, rec.clone())
		delete(r.open, key)
	}
	sortRecords(out)
	return out
}

func (r *RestartRecord) clone() RestartRecord {
	out := *r
	out.Reasons = maps.Clone(r.Reasons)
	if r.LastExitCode != nil {
		code := *r.LastExitCode
		out.LastExitCode = &code
	}
	return out
}

func sortRecords(records []RestartRecord) {
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
