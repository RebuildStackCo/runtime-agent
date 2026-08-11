package targeting

import (
	"strings"
	"testing"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeintake"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

func rec(ns, name string, cpu int64) *rollup.Record {
	var r rollup.Record
	r.Namespace, r.WorkloadKind, r.WorkloadName = ns, "Deployment", name
	r.CPU.CoreNanoseconds = cpu
	return &r
}

func names(ts []nodeintake.Target) string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.WorkloadName
	}
	return strings.Join(out, ",")
}

func TestPublisherRanksEligibleTopN(t *testing.T) {
	p := NewPublisher([]string{"shop"}, nil, 2)
	p.Publish([]*rollup.Record{
		rec("shop", "api", 100),
		rec("shop", "web", 300),
		rec("shop", "worker", 200),
		rec("infra", "proxy", 999), // ineligible namespace: excluded
	})
	if got := names(p.Snapshot()); got != "web,worker" {
		t.Errorf("targets = %q, want web,worker (top 2 by cpu, infra excluded)", got)
	}
}

func TestPublisherEmptyEligibleAdmitsNone(t *testing.T) {
	p := NewPublisher(nil, nil, 5)
	p.Publish([]*rollup.Record{rec("shop", "api", 100)})
	if s := p.Snapshot(); len(s) != 0 {
		t.Errorf("empty eligible set must admit none, got %q", names(s))
	}
}

func TestPublisherEligibleWorkloadsRestrict(t *testing.T) {
	p := NewPublisher([]string{"shop"}, []string{"api"}, 5)
	p.Publish([]*rollup.Record{
		rec("shop", "api", 50),
		rec("shop", "web", 100), // higher cpu but not in eligible workloads
	})
	if got := names(p.Snapshot()); got != "api" {
		t.Errorf("targets = %q, want api (eligibleWorkloads restricts)", got)
	}
}

func TestPublisherSumsPerWorkload(t *testing.T) {
	p := NewPublisher([]string{"shop"}, nil, 1)
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
	if s := NewPublisher([]string{"x"}, nil, 3).Snapshot(); s != nil {
		t.Errorf("Snapshot before Publish = %q, want nil", names(s))
	}
}
