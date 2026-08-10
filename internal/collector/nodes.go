package collector

import (
	"context"
	"fmt"
	"sort"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// NodeInfo is the collected view of one node: its size (allocatable and
// capacity, normalized to millicores and bytes), the two labels the cost model
// needs — instance type and capacity type — and the kernel version, which
// determines whether the node can run the eBPF profile (CAP_BPF needs kernel
// 5.8+). Nothing else is read from node objects (see docs/security.md §4).
type NodeInfo struct {
	Name                   string `json:"name"`
	InstanceType           string `json:"instance_type,omitempty"`
	CapacityType           string `json:"capacity_type,omitempty"` // "spot", "on-demand", or "" when undeterminable
	KernelVersion          string `json:"kernel_version,omitempty"`
	AllocatableCPUMilli    int64  `json:"allocatable_cpu_milli"`
	AllocatableMemoryBytes int64  `json:"allocatable_memory_bytes"`
	CapacityCPUMilli       int64  `json:"capacity_cpu_milli"`
	CapacityMemoryBytes    int64  `json:"capacity_memory_bytes"`
}

// NodeWatcher lists all nodes and keeps watching for new ones. Every observed
// node is reported through the onNode callback.
type NodeWatcher struct {
	clientset kubernetes.Interface
	onNode    func(NodeInfo)

	mu    sync.RWMutex
	names map[string]struct{}
}

// NewNodeWatcher returns a watcher that calls onNode for every node present
// at start and for every node added afterwards. onNode is called from the
// informer goroutine and must not block.
func NewNodeWatcher(clientset kubernetes.Interface, onNode func(NodeInfo)) *NodeWatcher {
	return &NodeWatcher{clientset: clientset, onNode: onNode, names: make(map[string]struct{})}
}

// Names returns the currently known node names, sorted. The usage poller
// uses it as its polling target list; before the informer syncs it is empty,
// which simply defers the first poll — cumulative counters lose nothing.
func (w *NodeWatcher) Names() []string {
	w.mu.RLock()
	out := make([]string, 0, len(w.names))
	for name := range w.names {
		out = append(out, name)
	}
	w.mu.RUnlock()
	sort.Strings(out)
	return out
}

// Run blocks until ctx is canceled.
func (w *NodeWatcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactory(w.clientset, 0)

	// Informers must be instantiated before Start, or the factory won't
	// run them.
	nodesInformer := factory.Core().V1().Nodes().Informer()

	factory.Start(ctx.Done())
	defer factory.Shutdown()

	if !cache.WaitForCacheSync(ctx.Done(), nodesInformer.HasSynced) {
		if ctx.Err() != nil {
			return nil // canceled during sync — a normal shutdown, not a failure
		}
		return fmt.Errorf("node informer cache did not sync")
	}

	reg, err := nodesInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if node, ok := obj.(*corev1.Node); ok {
				w.mu.Lock()
				w.names[node.Name] = struct{}{}
				w.mu.Unlock()
				w.onNode(describeNode(node))
			}
		},
		DeleteFunc: func(obj any) {
			if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = unknown.Obj
			}
			if node, ok := obj.(*corev1.Node); ok {
				w.mu.Lock()
				delete(w.names, node.Name)
				w.mu.Unlock()
			}
		},
	})
	if err != nil {
		return fmt.Errorf("register node handler: %w", err)
	}
	defer func() { _ = nodesInformer.RemoveEventHandler(reg) }()

	<-ctx.Done()
	return nil
}

// describeNode reduces a node to the collected view.
func describeNode(node *corev1.Node) NodeInfo {
	return NodeInfo{
		Name:                   node.Name,
		InstanceType:           instanceType(node.Labels),
		CapacityType:           capacityType(node.Labels),
		KernelVersion:          node.Status.NodeInfo.KernelVersion,
		AllocatableCPUMilli:    node.Status.Allocatable.Cpu().MilliValue(),
		AllocatableMemoryBytes: node.Status.Allocatable.Memory().Value(),
		CapacityCPUMilli:       node.Status.Capacity.Cpu().MilliValue(),
		CapacityMemoryBytes:    node.Status.Capacity.Memory().Value(),
	}
}

// instanceType reads the well-known instance-type label, falling back to the
// deprecated beta name still set by older clusters.
func instanceType(labels map[string]string) string {
	if t := labels[corev1.LabelInstanceTypeStable]; t != "" {
		return t
	}
	return labels[corev1.LabelInstanceType]
}

// capacityType normalizes the provider-specific spot/on-demand markers to
// "spot" or "on-demand". Nodes without any known marker return "" — absence
// of evidence is reported as unknown, not defaulted to on-demand.
func capacityType(labels map[string]string) string {
	// Karpenter sets the normalized value directly.
	switch labels["karpenter.sh/capacity-type"] {
	case "spot":
		return "spot"
	case "on-demand", "reserved":
		return "on-demand"
	}
	// EKS managed node groups.
	switch labels["eks.amazonaws.com/capacityType"] {
	case "SPOT":
		return "spot"
	case "ON_DEMAND":
		return "on-demand"
	}
	// GKE: spot VMs and legacy preemptible VMs.
	if labels["cloud.google.com/gke-spot"] == "true" || labels["cloud.google.com/gke-preemptible"] == "true" {
		return "spot"
	}
	// AKS scale set priority: "spot" or "regular".
	switch labels["kubernetes.azure.com/scalesetpriority"] {
	case "spot":
		return "spot"
	case "regular":
		return "on-demand"
	}
	return ""
}
