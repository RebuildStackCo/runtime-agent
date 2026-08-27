package nodescan

import "strings"

// DefaultInfraModulePrefixes is the built-in deny-list of Go module paths the
// node scanner drops before anything about them is recorded (invariant 4):
// cluster infrastructure — control plane, runtime, CNI, observability stack —
// plus this agent's own module. Matching is by prefix on a path boundary, so
// "k8s.io/" excludes "k8s.io/kubernetes" and not "k8s.io.example.com/...".
//
// Deliberately conservative: a reported infra binary is a harmless extra count.
// This is data reduction, not a security boundary.
var DefaultInfraModulePrefixes = []string{
	// This agent itself.
	"github.com/RebuildStackCo/",
	// Kubernetes control plane, kubelet, kube-proxy, client tooling.
	"k8s.io/",
	"sigs.k8s.io/",
	// Container runtime and low-level runtime plumbing.
	"github.com/containerd/",
	"github.com/opencontainers/",
	"github.com/cri-o/",
	// Cluster DNS.
	"github.com/coredns/",
	// CNI and service mesh.
	"github.com/containernetworking/",
	"github.com/cilium/",
	"github.com/projectcalico/",
	"github.com/tigera/",
	"istio.io/",
	"github.com/envoyproxy/",
	// Cluster datastore.
	"go.etcd.io/",
	// Observability stack.
	"github.com/prometheus/",
	"github.com/prometheus-operator/",
	"github.com/grafana/",
	"github.com/open-telemetry/",
	"github.com/fluent/",
}

// ModuleFilter decides whether a Go binary's main module path is customer
// workload (kept) or infrastructure (dropped on the node). It carries no
// state; counting lives in the scanner.
type ModuleFilter struct {
	prefixes []string
}

// NewModuleFilter builds a filter from a list of module-path prefixes to drop.
// A nil or empty list keeps everything. Pass DefaultInfraModulePrefixes for the
// built-in infrastructure deny-list.
func NewModuleFilter(dropPrefixes []string) *ModuleFilter {
	return &ModuleFilter{prefixes: dropPrefixes}
}

// IsInfra reports whether modulePath is on the deny-list. The empty module path
// (a binary built outside module mode, reported by the toolchain as
// "command-line-arguments" or blank) is not infra by this test — the scanner
// decides separately how to treat metadata it could not attribute.
func (f *ModuleFilter) IsInfra(modulePath string) bool {
	for _, p := range f.prefixes {
		if prefixOnBoundary(modulePath, p) {
			return true
		}
	}
	return false
}

// prefixOnBoundary reports whether path starts with prefix. Prefixes here end
// in "/", so this is an ordinary case-insensitive prefix test; case folding
// guards against the host/org casing differences that GitHub treats as equal
// ("RebuildStackCo" vs "rebuildstackco") while Go module paths preserve case.
func prefixOnBoundary(path, prefix string) bool {
	if len(path) < len(prefix) {
		return false
	}
	return strings.EqualFold(path[:len(prefix)], prefix)
}
