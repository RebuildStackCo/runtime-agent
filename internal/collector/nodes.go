package collector

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// NodeInfo is the collected view of one node: allocatable and capacity
// normalized to millicores and bytes, the labels the cost model needs (instance
// type, capacity type, zone, region), the kernel version that decides whether
// the eBPF profile can run, and the CPU architecture a build's GOARCH is
// compared against. Nothing else is read from node objects (security.md §4).
//
// Zone and region are join keys the cluster already publishes; the agent copies
// them and draws no conclusion from them.
type NodeInfo struct {
	Name          string `json:"name"`
	InstanceType  string `json:"instance_type,omitempty"`
	CapacityType  string `json:"capacity_type,omitempty"` // "spot", "on-demand", or "" when undeterminable
	Zone          string `json:"zone,omitempty"`
	Region        string `json:"region,omitempty"`
	KernelVersion string `json:"kernel_version,omitempty"`
	// Architecture is the node's CPU architecture as the kubelet reports it
	// ("amd64", "arm64"). It is what makes a build's GOARCH and microarchitecture
	// level answerable rather than merely recorded: a question about a binary's
	// target needs the machine it landed on (ADR 0019).
	Architecture           string `json:"architecture,omitempty"`
	AllocatableCPUMilli    int64  `json:"allocatable_cpu_milli"`
	AllocatableMemoryBytes int64  `json:"allocatable_memory_bytes"`
	CapacityCPUMilli       int64  `json:"capacity_cpu_milli"`
	CapacityMemoryBytes    int64  `json:"capacity_memory_bytes"`
}

// NodeWatcher lists all nodes and keeps watching for new ones. Every observed
// node is reported through the onNode callback, and the current view of every
// live node is kept for the node-metadata snapshot.
type NodeWatcher struct {
	clientset kubernetes.Interface
	onNode    func(NodeInfo)

	mu    sync.RWMutex
	nodes map[string]NodeInfo

	// The node list gates a signal rather than adding one: the usage poller
	// takes its target list from it, so a frozen cache stops new nodes from ever
	// being polled while the old ones are polled forever. A sustained failure
	// stops the agent (ADR 0035).
	limits watchLimits
	now    func() time.Time
}

// NewNodeWatcher returns a watcher that calls onNode for every node present
// at start and for every node added afterwards. onNode is called from the
// informer goroutine and must not block.
func NewNodeWatcher(clientset kubernetes.Interface, onNode func(NodeInfo)) *NodeWatcher {
	return &NodeWatcher{
		clientset: clientset,
		onNode:    onNode,
		nodes:     make(map[string]NodeInfo),
		limits:    defaultWatchLimits(),
		now:       time.Now,
	}
}

// clock is the watchdog's time source, defaulting to the wall clock for a
// watcher assembled by hand in a test.
func (w *NodeWatcher) clock() time.Time {
	if w.now == nil {
		return time.Now()
	}
	return w.now()
}

// Names returns the currently known node names, sorted. The usage poller
// uses it as its polling target list; before the informer syncs it is empty,
// which simply defers the first poll — cumulative counters lose nothing.
func (w *NodeWatcher) Names() []string {
	w.mu.RLock()
	out := make([]string, 0, len(w.nodes))
	for name := range w.nodes {
		out = append(out, name)
	}
	w.mu.RUnlock()
	sort.Strings(out)
	return out
}

// Nodes returns the current collected view of every live node, sorted by name
// so the payload bytes are deterministic (the golden contract,
// docs/development.md). Deleted nodes are absent: the snapshot is the current
// truth, rebuilt from the informer, never an append-only history.
func (w *NodeWatcher) Nodes() []NodeInfo {
	w.mu.RLock()
	out := make([]NodeInfo, 0, len(w.nodes))
	for _, n := range w.nodes {
		out = append(out, n)
	}
	w.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Run blocks until ctx is canceled.
func (w *NodeWatcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactory(w.clientset, 0)

	// Informers must be instantiated before Start, or the factory won't
	// run them.
	nodesInformer := factory.Core().V1().Nodes().Informer()
	gating := map[string]*watchHealth{
		"nodes": trackWatch(nodesInformer, w.limits.streakGap, w.clock),
	}

	// Started before the wait, and the informers run on runCtx, for the two
	// reasons spelled out in PodWatcher.Run: a cache refused from the first LIST
	// never syncs and the wait has no timeout, and Shutdown waits for every
	// informer it started.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	fatal := make(chan error, 1)
	go func() {
		if err := watchdog(runCtx, w.clock, w.limits, gating); err != nil {
			fatal <- err
			cancelRun()
		}
	}()

	factory.Start(runCtx.Done())
	defer factory.Shutdown()

	if !cache.WaitForCacheSync(runCtx.Done(), nodesInformer.HasSynced) {
		if err := takeWatchFailure(fatal); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil // canceled during sync — a normal shutdown, not a failure
		}
		return fmt.Errorf("node informer cache did not sync")
	}

	reg, err := nodesInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if node, ok := obj.(*corev1.Node); ok {
				w.upsert(node)
			}
		},
		// Nodes are updated constantly (conditions, heartbeats), and almost
		// none of it touches the collected view. upsert keeps the snapshot
		// current either way but only reports when the view actually changed —
		// a relabeled node or one resized in place must not go stale, and an
		// unchanged one must not spam the log.
		UpdateFunc: func(_, obj any) {
			if node, ok := obj.(*corev1.Node); ok {
				w.upsert(node)
			}
		},
		DeleteFunc: func(obj any) {
			if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = unknown.Obj
			}
			if node, ok := obj.(*corev1.Node); ok {
				w.mu.Lock()
				delete(w.nodes, node.Name)
				w.mu.Unlock()
			}
		},
	})
	if err != nil {
		return fmt.Errorf("register node handler: %w", err)
	}
	defer func() { _ = nodesInformer.RemoveEventHandler(reg) }()

	<-runCtx.Done()
	return takeWatchFailure(fatal)
}

// upsert stores the node's current collected view and reports it through
// onNode only when it differs from the view already held. NodeInfo is
// comparable, so "changed" is an ordinary equality test.
func (w *NodeWatcher) upsert(node *corev1.Node) {
	info := describeNode(node)
	w.mu.Lock()
	prev, existed := w.nodes[info.Name]
	w.nodes[info.Name] = info
	w.mu.Unlock()
	if !existed || prev != info {
		w.onNode(info)
	}
}

// describeNode reduces a node to the collected view.
func describeNode(node *corev1.Node) NodeInfo {
	return NodeInfo{
		Name:                   node.Name,
		InstanceType:           instanceType(node.Labels),
		CapacityType:           capacityType(node.Labels),
		Zone:                   topologyLabel(node.Labels, corev1.LabelTopologyZone, corev1.LabelFailureDomainBetaZone),
		Region:                 topologyLabel(node.Labels, corev1.LabelTopologyRegion, corev1.LabelFailureDomainBetaRegion),
		KernelVersion:          node.Status.NodeInfo.KernelVersion,
		Architecture:           node.Status.NodeInfo.Architecture,
		AllocatableCPUMilli:    node.Status.Allocatable.Cpu().MilliValue(),
		AllocatableMemoryBytes: node.Status.Allocatable.Memory().Value(),
		CapacityCPUMilli:       node.Status.Capacity.Cpu().MilliValue(),
		CapacityMemoryBytes:    node.Status.Capacity.Memory().Value(),
	}
}

// topologyLabel reads a well-known topology label, falling back to the
// deprecated beta name still set by older clusters and by some managed
// providers. An unlabeled node — bare metal, or a provider that publishes no
// topology — yields "", reported as unknown rather than invented.
func topologyLabel(labels map[string]string, stable, beta string) string {
	if v := labels[stable]; v != "" {
		return v
	}
	return labels[beta]
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
