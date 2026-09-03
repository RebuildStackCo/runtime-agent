package collector

import (
	"sort"
	"strings"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	corev1 "k8s.io/api/core/v1"
)

// The three reductions that turn the parts of a node object with unbounded
// shape — its conditions, its taints, its extended resources — into a bounded
// collected view (ADR 0064).
//
// Each follows the discipline placement.go established for operator-written
// strings: an allow-list rather than a deny-list, a length bound, and a drop
// counted rather than a value truncated (ADR 0019, ADR 0031).

// maxNodeTerms bounds how many taints or devices are kept from one node. Real
// nodes carry a handful; past this the node is not describing itself the way
// this reduction assumes.
const maxNodeTerms = 32

// nodeDrops counts what the reductions refused to carry. Aggregate: what was
// dropped is counted, never named (CLAUDE.md invariant 6).
type nodeDrops struct {
	// Conditions counts conditions refused for their type, Devices extended
	// resources refused for their vendor or past the term bound.
	Conditions int64
	Devices    int64
	// Taints counts taints dropped for an oversized key or value, and Values
	// the same for the scalar fields of the node view.
	Taints int64
	Values int64
}

// nodeConditionTypes is the closed set of conditions the agent reads: the four
// the kubelet maintains plus the one the network plugin sets. All five are
// reported whenever the node reports them, healthy or not — a condition list
// holding only problems would make "no problems" and "not read" the same bytes
// (ADR 0048 §2).
var nodeConditionTypes = map[corev1.NodeConditionType]bool{
	corev1.NodeReady:              true,
	corev1.NodeMemoryPressure:     true,
	corev1.NodeDiskPressure:       true,
	corev1.NodePIDPressure:        true,
	corev1.NodeNetworkUnavailable: true,
}

// deviceVendors is the allow-list of extended-resource prefixes, by hardware
// vendor. A prefix rather than exact names because one vendor's line is open —
// `nvidia.com/gpu` and `nvidia.com/mig-1g.5gb` are the same fact — and an
// allow-list rather than "every extended resource" because the rest of that
// namespace is operator-defined, where a resource named for an internal team or
// licence is an identity the agent must not carry out.
var deviceVendors = []string{
	"nvidia.com/",
	"amd.com/",
	"gpu.intel.com/",
	"habana.ai/",
	"aws.amazon.com/",
	"google.com/",
	"xilinx.com/",
}

// reduceConditions keeps the allow-listed conditions, sorted by type so the
// payload bytes are deterministic (the golden contract).
func reduceConditions(conditions []corev1.NodeCondition, drops *nodeDrops) []model.NodeCondition {
	var out []model.NodeCondition
	for _, c := range conditions {
		if !nodeConditionTypes[c.Type] {
			drops.Conditions++
			continue
		}
		kept := model.NodeCondition{Type: string(c.Type), Status: string(c.Status)}
		if fits(c.Reason) {
			kept.Reason = c.Reason
		} else if c.Reason != "" {
			drops.Values++
		}
		if !c.LastTransitionTime.IsZero() {
			kept.Since = c.LastTransitionTime.UTC()
		}
		out = append(out, kept)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// reduceTaints keeps every taint whose key and value fit the bound. Unlike
// tolerations, none is filtered as a cluster default: the two the node
// controller adds — `not-ready` and `unreachable` — appear only on a node that
// is actually broken, which is the signal rather than noise.
func reduceTaints(taints []corev1.Taint, drops *nodeDrops) []model.NodeTaint {
	var out []model.NodeTaint
	for _, t := range taints {
		if !fits(t.Key) || !fits(t.Value) {
			drops.Taints++
			continue
		}
		out = append(out, model.NodeTaint{Key: t.Key, Value: t.Value, Effect: string(t.Effect)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Effect < out[j].Effect
	})
	return capNodeTerms(out, &drops.Taints)
}

// reduceDevices reads the allow-listed extended resources out of the node's
// capacity and allocatable maps. A resource present in capacity but absent from
// allocatable reports zero allocatable, which is the true state: the hardware is
// there and the scheduler will not hand it out.
func reduceDevices(status corev1.NodeStatus, drops *nodeDrops) []model.NodeDevice {
	var out []model.NodeDevice
	for name, quantity := range status.Capacity {
		if !isExtendedResource(string(name)) {
			continue
		}
		if !isCollectedDevice(string(name)) {
			drops.Devices++
			continue
		}
		device := model.NodeDevice{Name: string(name), Capacity: quantity.Value()}
		if allocatable, ok := status.Allocatable[name]; ok {
			device.Allocatable = allocatable.Value()
		}
		out = append(out, device)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return capNodeTerms(out, &drops.Devices)
}

// isExtendedResource reports whether a resource name is one of the node's own
// advertised devices rather than a core resource. Core resources are unprefixed
// (`cpu`, `memory`, `pods`) or sit under `kubernetes.io/`, and every one the
// agent wants is already a named field.
func isExtendedResource(name string) bool {
	slash := strings.IndexByte(name, '/')
	if slash < 0 {
		return false
	}
	domain := name[:slash]
	return domain != "kubernetes.io" && !strings.HasSuffix(domain, ".kubernetes.io")
}

func isCollectedDevice(name string) bool {
	for _, vendor := range deviceVendors {
		if strings.HasPrefix(name, vendor) {
			return true
		}
	}
	return false
}

// capNodeTerms bounds a list, counting the excess rather than silently
// shortening it — capTerms in placement.go, with this package's counter.
func capNodeTerms[T any](terms []T, dropped *int64) []T {
	if len(terms) <= maxNodeTerms {
		return terms
	}
	*dropped += int64(len(terms) - maxNodeTerms)
	return terms[:maxNodeTerms]
}
