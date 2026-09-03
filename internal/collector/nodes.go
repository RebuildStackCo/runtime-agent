package collector

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// NodeInfo is the collected view of one node: how big it is, what kind of
// machine it is, where it sits, what software runs it, what state it is in and
// what it refuses to run. Nothing else is read from node objects
// (security.md §4).
//
// Zone and region are join keys the cluster already publishes; the agent copies
// them and draws no conclusion from them. The fields past Architecture arrived
// together in ADR 0064.
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
	Architecture string `json:"architecture,omitempty"`
	// CreatedAt is the node object's own creation timestamp. A fleet's age
	// distribution is what separates a cluster the autoscaler churns hourly from
	// one whose machines have been up since the last upgrade.
	CreatedAt time.Time `json:"created_at,omitzero"`
	// The rest of `status.nodeInfo` the agent reads. Kernel version above
	// already decides whether the eBPF profiler can run; these say what would
	// have to be upgraded to change that, and which runtime's behavior a
	// container is subject to.
	KubeletVersion   string `json:"kubelet_version,omitempty"`
	OSImage          string `json:"os_image,omitempty"`
	OperatingSystem  string `json:"operating_system,omitempty"`
	ContainerRuntime string `json:"container_runtime,omitempty"`

	AllocatableCPUMilli    int64 `json:"allocatable_cpu_milli"`
	AllocatableMemoryBytes int64 `json:"allocatable_memory_bytes"`
	CapacityCPUMilli       int64 `json:"capacity_cpu_milli"`
	CapacityMemoryBytes    int64 `json:"capacity_memory_bytes"`
	// The two core resources that also bound scheduling and that CPU and memory
	// do not imply: image and log space, and the kubelet's own pod ceiling. A
	// node idle in both CPU and memory can still be full (ADR 0064 §1).
	AllocatableEphemeralBytes int64 `json:"allocatable_ephemeral_storage_bytes,omitempty"`
	CapacityEphemeralBytes    int64 `json:"capacity_ephemeral_storage_bytes,omitempty"`
	AllocatablePods           int64 `json:"allocatable_pods,omitempty"`
	CapacityPods              int64 `json:"capacity_pods,omitempty"`

	// The three reduced lists (nodedetail.go). Each is absent when the node has
	// none, which for Conditions cannot happen on a live node — a node reporting
	// no allow-listed condition is one the kubelet has never contacted.
	Devices    []NodeDevice    `json:"devices,omitempty"`
	Conditions []NodeCondition `json:"conditions,omitempty"`
	Taints     []NodeTaint     `json:"taints,omitempty"`
}

// sameAs reports whether two collected views are the same fact, replacing the
// `==` this type carried while every field was scalar. Reflection rather than a
// field list, so a field added above joins the test by existing; affordable
// because node objects change on the order of minutes — the kubelet heartbeats
// to a Lease, and the one sub-second field it does write, the condition
// heartbeat, is deliberately not collected. The lists are built sorted, so
// element-wise equality is exact rather than merely sufficient.
func (n NodeInfo) sameAs(other NodeInfo) bool { return reflect.DeepEqual(n, other) }

// NodeLifecycle is one node arriving in or leaving the cluster (ADR 0064 §3).
// It carries the node's collected view because a departure is the last moment
// that view exists: once the object is gone, `node_metadata` no longer holds the
// row, and the size of what left would be unknowable.
type NodeLifecycle struct {
	Node NodeInfo
	// Joined distinguishes the two events. A join is only reported when it can
	// be proved — see reportJoin.
	Joined bool
	// At is the node object's creation timestamp for a join, and the instant the
	// agent noticed for a departure. Observed says which.
	At       time.Time
	Observed bool
}

// NodeWatcher lists all nodes and keeps watching for new ones. Every observed
// node is reported through the onNode callback, and the current view of every
// live node is kept for the node-metadata snapshot.
type NodeWatcher struct {
	clientset   kubernetes.Interface
	onNode      func(NodeInfo)
	onLifecycle func(NodeLifecycle)

	// since bounds what counts as a join: a node whose object predates it was
	// already in the cluster when this process started watching, and reporting
	// it as an arrival would invent fleet growth out of an agent restart
	// (ADR 0034's rule, applied to nodes).
	since time.Time

	conditionsDropped atomic.Int64
	devicesDropped    atomic.Int64
	taintsDropped     atomic.Int64
	valuesDropped     atomic.Int64

	// afterSync is the test seam described where the type is declared.
	afterSync afterSync

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
		since:     time.Now().UTC(),
	}
}

// OnNodeLifecycle registers fn to be called once per node arrival or departure.
// Must be called before Run. fn is called from the informer goroutine and must
// not block.
func (w *NodeWatcher) OnNodeLifecycle(fn func(NodeLifecycle)) {
	w.onLifecycle = fn
}

// NodeDrops is what the node reductions refused to carry, cumulative since the
// process started. It reaches the coverage report so a fleet whose nodes do not
// fit these bounds is visible rather than quietly under-described.
type NodeDrops struct {
	Conditions int64 `json:"conditions_dropped"`
	Devices    int64 `json:"devices_dropped"`
	Taints     int64 `json:"taints_dropped"`
	Values     int64 `json:"values_dropped"`
}

// Drops returns the running totals.
func (w *NodeWatcher) Drops() NodeDrops {
	return NodeDrops{
		Conditions: w.conditionsDropped.Load(),
		Devices:    w.devicesDropped.Load(),
		Taints:     w.taintsDropped.Load(),
		Values:     w.valuesDropped.Load(),
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
	if w.afterSync != nil {
		w.afterSync(nodesInformer)
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
				w.reportDeparture(node.Name)
			}
		},
	})
	if err != nil {
		return registrationFailure(ctx, fatal, "node", err)
	}
	defer func() { _ = nodesInformer.RemoveEventHandler(reg) }()

	<-runCtx.Done()
	return takeWatchFailure(fatal)
}

// upsert stores the node's current collected view and reports it through
// onNode only when it differs from the view already held.
func (w *NodeWatcher) upsert(node *corev1.Node) {
	info, drops := describeNode(node)
	w.conditionsDropped.Add(drops.Conditions)
	w.devicesDropped.Add(drops.Devices)
	w.taintsDropped.Add(drops.Taints)
	w.valuesDropped.Add(drops.Values)

	w.mu.Lock()
	prev, existed := w.nodes[info.Name]
	w.nodes[info.Name] = info
	w.mu.Unlock()
	if !existed {
		w.reportJoin(info)
	}
	if !existed || !prev.sameAs(info) {
		w.onNode(info)
	}
}

// reportJoin reports a node that arrived after this process started watching.
//
// The creation timestamp is the test, not the fact that the informer had not
// seen the node before: at cache sync the add handler fires for every node in
// the cluster, and counting those would report the whole fleet as having
// arrived at the moment the agent booted.
func (w *NodeWatcher) reportJoin(info NodeInfo) {
	if w.onLifecycle == nil || info.CreatedAt.IsZero() || !info.CreatedAt.After(w.since) {
		return
	}
	w.onLifecycle(NodeLifecycle{Node: info, Joined: true, At: info.CreatedAt})
}

// reportDeparture reports a node that left. Unlike a join it needs no proof —
// the object existed and is gone — but it also carries no time of its own: a
// deleted object is simply absent, so the moment is the agent's observation and
// the record says so.
func (w *NodeWatcher) reportDeparture(name string) {
	w.mu.Lock()
	info, existed := w.nodes[name]
	delete(w.nodes, name)
	w.mu.Unlock()
	if !existed || w.onLifecycle == nil {
		return
	}
	w.onLifecycle(NodeLifecycle{Node: info, At: w.clock().UTC(), Observed: true})
}

// describeNode reduces a node to the collected view, returning what the three
// bounded reductions refused to carry alongside it.
func describeNode(node *corev1.Node) (NodeInfo, nodeDrops) {
	var drops nodeDrops
	info := NodeInfo{
		Name:                   node.Name,
		InstanceType:           instanceType(node.Labels),
		CapacityType:           capacityType(node.Labels),
		Zone:                   topologyLabel(node.Labels, corev1.LabelTopologyZone, corev1.LabelFailureDomainBetaZone),
		Region:                 topologyLabel(node.Labels, corev1.LabelTopologyRegion, corev1.LabelFailureDomainBetaRegion),
		KernelVersion:          node.Status.NodeInfo.KernelVersion,
		Architecture:           node.Status.NodeInfo.Architecture,
		KubeletVersion:         node.Status.NodeInfo.KubeletVersion,
		OSImage:                node.Status.NodeInfo.OSImage,
		OperatingSystem:        node.Status.NodeInfo.OperatingSystem,
		ContainerRuntime:       node.Status.NodeInfo.ContainerRuntimeVersion,
		AllocatableCPUMilli:    node.Status.Allocatable.Cpu().MilliValue(),
		AllocatableMemoryBytes: node.Status.Allocatable.Memory().Value(),
		CapacityCPUMilli:       node.Status.Capacity.Cpu().MilliValue(),
		CapacityMemoryBytes:    node.Status.Capacity.Memory().Value(),

		AllocatableEphemeralBytes: node.Status.Allocatable.StorageEphemeral().Value(),
		CapacityEphemeralBytes:    node.Status.Capacity.StorageEphemeral().Value(),
		AllocatablePods:           node.Status.Allocatable.Pods().Value(),
		CapacityPods:              node.Status.Capacity.Pods().Value(),

		Devices:    reduceDevices(node.Status, &drops),
		Conditions: reduceConditions(node.Status.Conditions, &drops),
		Taints:     reduceTaints(node.Spec.Taints, &drops),
	}
	if !node.CreationTimestamp.IsZero() {
		info.CreatedAt = node.CreationTimestamp.UTC()
	}
	// The version strings are the only free-form ones here: a kubelet or runtime
	// version is whatever the distribution stamped in. The rest are enums or
	// numbers the API server validates.
	for _, field := range []*string{&info.KubeletVersion, &info.OSImage, &info.OperatingSystem, &info.ContainerRuntime} {
		if !fits(*field) {
			*field = ""
			drops.Values++
		}
	}
	return info, drops
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
