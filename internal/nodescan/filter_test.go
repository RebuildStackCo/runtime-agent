package nodescan

import "testing"

func TestModuleFilterDefaults(t *testing.T) {
	f := NewModuleFilter(DefaultInfraModulePrefixes)

	infra := []string{
		"k8s.io/kubernetes",
		"sigs.k8s.io/controller-runtime",
		"github.com/containerd/containerd",
		"github.com/prometheus/prometheus",
		"github.com/grafana/loki",
		"go.etcd.io/etcd/server/v3",
		"istio.io/istio",
		// This agent's own module, in the canonical GitHub casing and a
		// lower-cased variant — both must be dropped.
		"github.com/RebuildStackCo/runtime-agent",
		"github.com/rebuildstackco/runtime-agent",
	}
	for _, m := range infra {
		if !f.IsInfra(m) {
			t.Errorf("IsInfra(%q) = false, want true", m)
		}
	}

	kept := []string{
		"example.com/team/payments",
		"github.com/acme/checkout",
		// A near-miss that must not be swallowed by the "k8s.io/" prefix.
		"k8s.io.evil.example.com/pkg",
		"",
	}
	for _, m := range kept {
		if f.IsInfra(m) {
			t.Errorf("IsInfra(%q) = true, want false", m)
		}
	}
}

func TestModuleFilterEmptyKeepsEverything(t *testing.T) {
	f := NewModuleFilter(nil)
	if f.IsInfra("k8s.io/kubernetes") {
		t.Error("empty filter should keep k8s.io/kubernetes")
	}
}
