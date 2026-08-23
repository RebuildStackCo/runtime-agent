package targeting

import (
	"strings"
	"testing"

	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

func rec(ns, name string, cpu int64) *rollup.Record {
	var r rollup.Record
	r.Namespace, r.WorkloadKind, r.WorkloadName = ns, "Deployment", name
	r.CPU.CoreNanoseconds = cpu
	return &r
}

func names(ts []Target) string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.WorkloadName
	}
	return strings.Join(out, ",")
}

func TestPublisherRanksTopN(t *testing.T) {
	p := NewPublisher(2)
	p.Publish([]*rollup.Record{
		rec("shop", "api", 100),
		rec("shop", "web", 300),
		rec("shop", "worker", 200),
	})
	if got := names(p.Snapshot()); got != "web,worker" {
		t.Errorf("targets = %q, want web,worker (top 2 by cpu)", got)
	}
}

// The bound on what may be ranked is the input, not a second filter: records
// reach Publish only for pods the collection filters admitted, so a workload the
// customer excluded was never measured and cannot appear here (ADR 0025).
func TestPublisherRanksWhateverWasCollected(t *testing.T) {
	p := NewPublisher(5)
	p.Publish([]*rollup.Record{
		rec("shop", "api", 100),
		rec("infra", "proxy", 999),
	})
	if got := names(p.Snapshot()); got != "proxy,api" {
		t.Errorf("targets = %q, want proxy,api — the publisher ranks what it is given", got)
	}
}

func TestPublisherSumsPerWorkload(t *testing.T) {
	p := NewPublisher(1)
	p.Publish([]*rollup.Record{
		rec("shop", "api", 100),
		rec("shop", "api", 100), // same workload -> 200
		rec("shop", "web", 150),
	})
	if got := names(p.Snapshot()); got != "api" {
		t.Errorf("targets = %q, want api (summed 200 > web 150)", got)
	}
}

func TestPublisherSnapshotBeforePublish(t *testing.T) {
	if s := NewPublisher(3).Snapshot(); s != nil {
		t.Errorf("Snapshot before Publish = %q, want nil", names(s))
	}
}
