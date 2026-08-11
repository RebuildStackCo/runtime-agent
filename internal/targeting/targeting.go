// Package targeting computes the top-N workloads to profile from the usage
// rollups and publishes them for the node targets query (ADR 0011 §3, §6b).
//
// It exists to keep the query handler off the usage Accumulator: the Accumulator
// is owned by the poller goroutine and has no synchronization, so the HTTP
// handler must never read it. Instead the poller calls Publish on its own tick
// with an already-deep-copied snapshot, and the handler reads only the atomically
// published result. The publisher also enforces the config bound — the published
// list is already filtered to the eligible set and capped at TopN — so the reply
// can never name a workload the operator did not permit.
package targeting

import (
	"sort"
	"sync/atomic"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeintake"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// Publisher ranks eligible workloads by CPU consumption and publishes the top N.
type Publisher struct {
	eligibleNS map[string]struct{}
	eligibleWL map[string]struct{} // empty => every workload in an eligible namespace
	topN       int
	current    atomic.Pointer[[]nodeintake.Target]
}

// NewPublisher builds a publisher bound to the operator's eligible set and TopN.
// An empty eligibleNamespaces admits nothing (deny-by-default); an empty
// eligibleWorkloads admits every workload within the eligible namespaces.
func NewPublisher(eligibleNamespaces, eligibleWorkloads []string, topN int) *Publisher {
	if topN <= 0 {
		topN = 1
	}
	return &Publisher{
		eligibleNS: toSet(eligibleNamespaces),
		eligibleWL: toSet(eligibleWorkloads),
		topN:       topN,
	}
}

// Publish ranks records by CPU consumption, keeps only eligible workloads, takes
// the top N, and publishes the result. It is called from the usage poller's
// snapshot callback, where records are already a deep copy — so it never races
// the Accumulator. Consumption is summed per workload across the records.
func (p *Publisher) Publish(records []*rollup.Record) {
	sum := make(map[nodeintake.Target]int64)
	for _, r := range records {
		if !p.eligible(r.Namespace, r.WorkloadName) {
			continue
		}
		t := nodeintake.Target{
			Namespace:    r.Namespace,
			WorkloadKind: r.WorkloadKind,
			WorkloadName: r.WorkloadName,
		}
		sum[t] += r.CPU.CoreNanoseconds
	}

	type ranked struct {
		t nodeintake.Target
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
	top := make([]nodeintake.Target, 0, n)
	for i := range n {
		top = append(top, rs[i].t)
	}
	p.current.Store(&top)
}

// Snapshot returns the currently published targets, or nil before the first
// Publish. It is safe to call from the request goroutine.
func (p *Publisher) Snapshot() []nodeintake.Target {
	if v := p.current.Load(); v != nil {
		return *v
	}
	return nil
}

func (p *Publisher) eligible(namespace, workload string) bool {
	if _, ok := p.eligibleNS[namespace]; !ok {
		return false
	}
	if len(p.eligibleWL) == 0 {
		return true
	}
	_, ok := p.eligibleWL[workload]
	return ok
}

func toSet(xs []string) map[string]struct{} {
	m := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		m[x] = struct{}{}
	}
	return m
}
