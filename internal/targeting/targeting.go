// Package targeting computes the top-N workloads to profile from the usage
// rollups and publishes them for the node targets query (ADR 0011 §3, §6b).
//
// It keeps the query handler off the usage Accumulator, which the poller
// goroutine owns without synchronization: the poller publishes a deep copy on
// its own tick and the handler reads only that. What bounds the list is the
// input, not a second filter — rollups exist only for pods the collection
// filters admitted, so profiling scope is collection scope (ADR 0025).
package targeting

import (
	"sort"
	"sync/atomic"

	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// Target names one workload the controller has ranked worth profiling. It is a
// controller-internal identifier: the node cannot act on it (it can't resolve a
// container to a workload), so it never goes on the wire — the controller
// expands the published targets to node-scoped container IDs at query time.
type Target struct {
	Namespace    string
	WorkloadKind string
	WorkloadName string
}

// Publisher ranks collected workloads by CPU consumption and publishes the top N.
type Publisher struct {
	topN     int
	optedOut func() map[Target]struct{}
	current  atomic.Pointer[[]Target]
}

// NewPublisher builds a publisher capped at topN; 0 or less selects one.
//
// optedOut names the workloads the profiling opt-out excludes, and may be nil.
// They are dropped before the ranking rather than after it: an excluded
// workload would otherwise take one of the topN places and yield no containers,
// spending the ceiling on nothing (ADR 0071).
func NewPublisher(topN int, optedOut func() map[Target]struct{}) *Publisher {
	if topN <= 0 {
		topN = 1
	}
	return &Publisher{topN: topN, optedOut: optedOut}
}

// Publish ranks records by CPU consumption, takes the top N, and publishes the
// result. It is called from the usage poller's snapshot callback, where records
// are already a deep copy — so it never races the Accumulator. Consumption is
// summed per workload across the records.
func (p *Publisher) Publish(records []*rollup.Record) {
	var excluded map[Target]struct{}
	if p.optedOut != nil {
		excluded = p.optedOut()
	}
	sum := make(map[Target]int64)
	for _, r := range records {
		t := Target{
			Namespace:    r.Namespace,
			WorkloadKind: r.WorkloadKind,
			WorkloadName: r.WorkloadName,
		}
		if _, ok := excluded[t]; ok {
			continue
		}
		sum[t] += r.CPU.CoreNanoseconds
	}

	type ranked struct {
		t Target
		v int64
	}
	rs := make([]ranked, 0, len(sum))
	for t, v := range sum {
		rs = append(rs, ranked{t, v})
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].v != rs[j].v {
			return rs[i].v > rs[j].v
		}
		// Deterministic tie-break so equal-consumption workloads publish stably.
		if rs[i].t.Namespace != rs[j].t.Namespace {
			return rs[i].t.Namespace < rs[j].t.Namespace
		}
		return rs[i].t.WorkloadName < rs[j].t.WorkloadName
	})

	n := min(p.topN, len(rs))
	top := make([]Target, 0, n)
	for i := range n {
		top = append(top, rs[i].t)
	}
	p.current.Store(&top)
}

// Snapshot returns the currently published targets, or nil before the first
// Publish. It is safe to call from the request goroutine.
func (p *Publisher) Snapshot() []Target {
	if v := p.current.Load(); v != nil {
		return *v
	}
	return nil
}
