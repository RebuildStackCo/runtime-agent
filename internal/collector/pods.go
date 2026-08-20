// Package collector observes cluster workloads. It reads and reduces; it
// never mutates cluster state.
package collector

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	batchlisters "k8s.io/client-go/listers/batch/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

// WorkloadRef identifies the controller that ultimately manages a pod:
// Deployment, StatefulSet, DaemonSet, CronJob, a bare Job, an Argo Rollout,
// or any other CRD that owns pods through the standard owner-reference chain.
// Kind is "none" for pods with no controller.
type WorkloadRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Resources is the declared resource envelope of one container, normalized
// to resource units: CPU in millicores, memory in bytes. A nil field means
// the corresponding request or limit is not set — a meaningful fact in
// itself, distinct from zero.
type Resources struct {
	CPURequestMilli    *int64 `json:"cpu_request_milli,omitempty"`
	CPULimitMilli      *int64 `json:"cpu_limit_milli,omitempty"`
	MemoryRequestBytes *int64 `json:"memory_request_bytes,omitempty"`
	MemoryLimitBytes   *int64 `json:"memory_limit_bytes,omitempty"`
}

// ContainerPort is a declared port from the pod spec — the fact that a
// container announces it, and nothing about whether it is ever used. Declared
// ports are how the controller locates pprof endpoints without blind scans
// (docs/security.md §4). Name and Protocol are omitted when unset.
type ContainerPort struct {
	Name     string `json:"name,omitempty"`
	Port     int32  `json:"port"`
	Protocol string `json:"protocol,omitempty"`
}

// Container is the collected view of a container: name, image, the image
// digest once the container has started, declared resources, and declared
// ports. Env, args, and command are deliberately never read (filter early).
type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// ImageDigest is the content digest (e.g. "sha256:…") the kubelet reports
	// for the running image. It is empty until the container starts, because
	// the runtime only knows it after pulling the image — see describe.
	ImageDigest string          `json:"image_digest,omitempty"`
	Init        bool            `json:"init,omitempty"`
	Resources   Resources       `json:"resources"`
	Ports       []ContainerPort `json:"ports,omitempty"`
}

// PodInfo is the collected view of one pod.
type PodInfo struct {
	Namespace  string
	Name       string
	Node       string
	Phase      string
	QOSClass   string
	Workload   WorkloadRef
	Containers []Container
}

// PodWatcher lists all pods in all namespaces and then keeps watching for
// new ones. Every observed pod is reported through the OnPod callback,
// resolved to the workload that manages it.
type PodWatcher struct {
	clientset kubernetes.Interface
	onPod     func(PodInfo)
	onOOM     func(OOMKill)
	filter    *PodFilter

	rsLister  appslisters.ReplicaSetLister
	jobLister batchlisters.JobLister
	nsLister  corelisters.NamespaceLister

	mu           sync.Mutex
	reportedOOMs map[string]struct{}
	// reportedSig deduplicates pod reports across the many status updates a
	// pod receives: the collected view is re-sent only when its image-digest
	// signature changes (digests appear on the update that follows container
	// start, never on the initial add). Losing the map on restart is harmless
	// — the pod is simply reported once more.
	reportedSig map[types.UID]string

	// index maps admitted pods to what the usage poller needs to attribute
	// kubelet samples. Excluded pods are never in it — absence means "drop
	// the sample at the source" (filter early). nameIndex carries the same
	// pods keyed by namespace/name, for sources that label by name only
	// (the cAdvisor exposition).
	indexMu   sync.RWMutex
	index     map[types.UID]podIndexEntry
	nameIndex map[string]types.UID
}

type podIndexEntry struct {
	namespace string
	name      string
	node      string
	workload  WorkloadRef
	// containers maps each started container's runtime ID (normalized: no
	// runtime prefix, lowercase) to its name and image digest. It is what the
	// node role's build-info facts join against — the node reports a pod UID
	// and container ID, the controller resolves them to a workload and the
	// image digest it already collects (ADR 0010).
	containers map[string]containerIdentity
	// info is the pod's full collected view, kept so the workload-metadata
	// snapshot is built from the same index that gates every other pod-derived
	// signal: one admission decision, one lifetime, no second source of truth.
	info PodInfo
}

// containerIdentity is what a container ID resolves to within its pod.
type containerIdentity struct {
	name        string
	imageDigest string
}

// NewPodWatcher returns a watcher that calls onPod for every pod present at
// start and for every pod created afterwards. onPod is called from the
// informer goroutine and must not block.
func NewPodWatcher(clientset kubernetes.Interface, onPod func(PodInfo)) *PodWatcher {
	return &PodWatcher{
		clientset:    clientset,
		onPod:        onPod,
		filter:       NewPodFilter(nil, nil),
		reportedOOMs: make(map[string]struct{}),
		reportedSig:  make(map[types.UID]string),
		index:        make(map[types.UID]podIndexEntry),
		nameIndex:    make(map[string]types.UID),
	}
}

// SetFilter replaces the default collect-everything filter. Must be called
// before Run. The filter gates every pod-derived signal: an excluded pod is
// neither reported nor scanned for OOM kills.
func (w *PodWatcher) SetFilter(filter *PodFilter) {
	w.filter = filter
}

// Run blocks until ctx is canceled. ReplicaSet and Job caches are synced
// before pod events are delivered, so owner chains resolve from the first
// event on.
func (w *PodWatcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactory(w.clientset, 0)
	rs := factory.Apps().V1().ReplicaSets()
	jobs := factory.Batch().V1().Jobs()
	namespaces := factory.Core().V1().Namespaces()
	pods := factory.Core().V1().Pods()
	w.rsLister = rs.Lister()
	w.jobLister = jobs.Lister()
	w.nsLister = namespaces.Lister()

	// Informers must be instantiated before Start, or the factory won't
	// run them.
	podsInformer := pods.Informer()
	rsSynced, jobsSynced := rs.Informer().HasSynced, jobs.Informer().HasSynced
	nsSynced := namespaces.Informer().HasSynced

	factory.Start(ctx.Done())
	defer factory.Shutdown()

	if !cache.WaitForCacheSync(ctx.Done(), rsSynced, jobsSynced, nsSynced, podsInformer.HasSynced) {
		if ctx.Err() != nil {
			return nil // canceled during sync — a normal shutdown, not a failure
		}
		return fmt.Errorf("informer caches did not sync")
	}

	// Registered after sync: the handler replays every pod already in the
	// cache as an Add, then receives genuinely new pods as they appear.
	reg, err := podsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if pod, ok := obj.(*corev1.Pod); ok && w.admit(pod, true) {
				info := w.describe(pod)
				w.indexPod(pod, info)
				w.reportPodIfChanged(pod.UID, info)
				w.reportOOMKills(pod)
			}
		},
		// Status updates carry facts absent from the initial add — OOM
		// kills, and container image digests, which the runtime only knows
		// after starting the container. The pod is re-reported when its
		// digest signature changes (reportPodIfChanged dedups the frequent
		// no-op updates). Admission is re-evaluated so an opt-out annotation
		// added mid-flight takes effect immediately.
		UpdateFunc: func(_, obj any) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return
			}
			if w.admit(pod, false) {
				info := w.describe(pod)
				w.indexPod(pod, info)
				w.reportPodIfChanged(pod.UID, info)
				w.reportOOMKills(pod)
			} else {
				w.dropPod(pod.UID)
			}
		},
		DeleteFunc: func(obj any) {
			if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = unknown.Obj
			}
			if pod, ok := obj.(*corev1.Pod); ok {
				w.dropPod(pod.UID)
				w.forgetOOMKills(pod)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("register pod handler: %w", err)
	}
	defer func() { _ = podsInformer.RemoveEventHandler(reg) }()

	<-ctx.Done()
	return nil
}

// indexPod records an admitted pod for sample attribution. info is the pod's
// already-described collected view, which the index also keeps: it is the
// source of the workload-metadata snapshot, so the snapshot covers exactly the
// pods that passed the filter and drops a pod the moment the index does.
func (w *PodWatcher) indexPod(pod *corev1.Pod, info PodInfo) {
	entry := podIndexEntry{
		namespace:  info.Namespace,
		name:       info.Name,
		node:       info.Node,
		workload:   info.Workload,
		containers: containerIndex(pod),
		info:       info,
	}
	w.indexMu.Lock()
	w.index[pod.UID] = entry
	w.nameIndex[info.Namespace+"/"+info.Name] = pod.UID
	w.indexMu.Unlock()
}

// Pods returns the collected view of every admitted, currently indexed pod,
// sorted by namespace and name so downstream aggregation is deterministic (the
// golden contract, docs/development.md). Excluded and deleted pods are absent —
// the snapshot is the current truth, never an append-only history. Container
// slices are copied, so callers may retain and reorder the result.
func (w *PodWatcher) Pods() []PodInfo {
	w.indexMu.RLock()
	out := make([]PodInfo, 0, len(w.index))
	for _, entry := range w.index {
		info := entry.info
		info.Containers = append([]Container(nil), entry.info.Containers...)
		out = append(out, info)
	}
	w.indexMu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// containerIndex maps each started container's normalized runtime ID to its
// name and image digest, from the pod's status. Containers that have not yet
// started (no runtime ID) are absent — they are unattributable until the
// runtime assigns an ID, and the node scanner cannot see them running either.
func containerIndex(pod *corev1.Pod) map[string]containerIdentity {
	idx := make(map[string]containerIdentity)
	add := func(statuses []corev1.ContainerStatus) {
		for _, s := range statuses {
			cid := normalizeContainerID(s.ContainerID)
			if cid == "" {
				continue
			}
			idx[cid] = containerIdentity{name: s.Name, imageDigest: parseImageDigest(s.ImageID)}
		}
	}
	add(pod.Status.InitContainerStatuses)
	add(pod.Status.ContainerStatuses)
	return idx
}

// normalizeContainerID strips the runtime scheme prefix a container status
// carries ("containerd://", "cri-o://", "docker://") and lowercases the digest,
// yielding the bare 64-hex form the node role parses from the process cgroup
// (ADR 0009). An empty or unset ID normalizes to "".
func normalizeContainerID(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+len("://"):]
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

func (w *PodWatcher) dropPod(uid types.UID) {
	w.indexMu.Lock()
	if entry, ok := w.index[uid]; ok {
		nameKey := entry.namespace + "/" + entry.name
		// A recreated pod reuses the name with a new UID; a late delete of
		// the old UID must not evict the new pod's name entry.
		if w.nameIndex[nameKey] == uid {
			delete(w.nameIndex, nameKey)
		}
		delete(w.index, uid)
	}
	w.indexMu.Unlock()

	w.mu.Lock()
	delete(w.reportedSig, uid)
	w.mu.Unlock()
}

// LookupPod resolves a pod UID to its namespace and workload. It reports
// false for pods that are unknown or excluded by the filter: the usage
// poller consults it before accumulating any kubelet sample, so an excluded
// pod's usage is dropped at the source, and an unknown pod's sample is
// deferred — cumulative counters make the retry lossless.
func (w *PodWatcher) LookupPod(uid types.UID) (namespace string, workload WorkloadRef, ok bool) {
	w.indexMu.RLock()
	entry, ok := w.index[uid]
	w.indexMu.RUnlock()
	return entry.namespace, entry.workload, ok
}

// LookupPodByName is LookupPod for sources that identify pods by
// namespace and name instead of UID.
func (w *PodWatcher) LookupPodByName(namespace, name string) (workload WorkloadRef, ok bool) {
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()
	uid, ok := w.nameIndex[namespace+"/"+name]
	if !ok {
		return WorkloadRef{}, false
	}
	entry, ok := w.index[uid]
	return entry.workload, ok
}

// LookupContainer resolves a node-role build-info fact — a pod UID and a
// container ID — to the workload, container name, and image digest the
// controller has for it (ADR 0010). It reports false when the pod is unknown or
// excluded (informer lag or a filtered pod), or when the container ID is not
// among the pod's started containers; the caller counts those as unjoined and
// drops them, never guessing. The container ID is matched in the node's bare,
// lowercased form.
func (w *PodWatcher) LookupContainer(podUID types.UID, containerID string) (namespace string, workload WorkloadRef, container, imageDigest string, ok bool) {
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()
	entry, ok := w.index[podUID]
	if !ok {
		return "", WorkloadRef{}, "", "", false
	}
	ci, ok := entry.containers[normalizeContainerID(containerID)]
	if !ok {
		return "", WorkloadRef{}, "", "", false
	}
	return entry.namespace, entry.workload, ci.name, ci.imageDigest, true
}

// ContainerOnNode is one running container attributed to its workload. It scopes
// a node's profiling targets (ADR 0011 §3): the controller expands the published
// top-N workloads to the containers of their pods on a given node, because the
// node cannot resolve a container to a workload itself (no API access, ADR 0009).
type ContainerOnNode struct {
	Namespace   string
	Workload    WorkloadRef
	ContainerID string
}

// ContainersOnNode returns every indexed container whose pod is scheduled on
// node. It is how the controller answers a node's targets query with container
// IDs the node can act on.
func (w *PodWatcher) ContainersOnNode(node string) []ContainerOnNode {
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()
	var out []ContainerOnNode
	for _, entry := range w.index {
		if entry.node != node {
			continue
		}
		for cid := range entry.containers {
			out = append(out, ContainerOnNode{
				Namespace:   entry.namespace,
				Workload:    entry.workload,
				ContainerID: cid,
			})
		}
	}
	return out
}

// admit runs the pod through the filter, consulting the namespace's
// annotations from the cache (a cache miss reads as no annotations).
// Exclusions are counted only when count is set — once per pod appearance
// on Add, never again on the many status updates that follow.
func (w *PodWatcher) admit(pod *corev1.Pod, count bool) bool {
	var nsAnnotations map[string]string
	if ns, err := w.nsLister.Get(pod.Namespace); err == nil {
		nsAnnotations = ns.Annotations
	}
	allowed, reason := w.filter.Admit(pod, nsAnnotations)
	if count {
		if allowed {
			w.filter.countObserved()
		} else {
			w.filter.countExcluded(reason)
		}
	}
	return allowed
}

// describe reduces a pod to the collected view.
func (w *PodWatcher) describe(pod *corev1.Pod) PodInfo {
	info := PodInfo{
		Namespace: pod.Namespace,
		Name:      pod.Name,
		Node:      pod.Spec.NodeName,
		Phase:     string(pod.Status.Phase),
		QOSClass:  string(pod.Status.QOSClass),
		Workload:  w.resolveWorkload(pod),
	}
	digests := containerDigests(pod)
	for _, c := range pod.Spec.InitContainers {
		info.Containers = append(info.Containers, Container{
			Name: c.Name, Image: c.Image, Init: true,
			ImageDigest: digests[c.Name], Resources: resourcesOf(&c), Ports: portsOf(&c),
		})
	}
	for _, c := range pod.Spec.Containers {
		info.Containers = append(info.Containers, Container{
			Name: c.Name, Image: c.Image,
			ImageDigest: digests[c.Name], Resources: resourcesOf(&c), Ports: portsOf(&c),
		})
	}
	return info
}

// reportPodIfChanged reports the pod through onPod, but only when its collected
// view has materially changed since the last report — specifically when a
// container's image digest first appears or changes. imageID is populated only
// after the kubelet pulls and starts the container, so it arrives on a status
// update rather than the initial add; without re-reporting, the digest would
// never reach a consumer. Deduplicating on the digest signature keeps the many
// unrelated status updates (readiness, conditions) from re-reporting an
// otherwise unchanged pod.
func (w *PodWatcher) reportPodIfChanged(uid types.UID, info PodInfo) {
	sig := digestSignature(info.Containers)
	w.mu.Lock()
	prev, seen := w.reportedSig[uid]
	changed := !seen || prev != sig
	if changed {
		w.reportedSig[uid] = sig
	}
	w.mu.Unlock()
	if changed {
		w.onPod(info)
	}
}

// digestSignature is a compact fingerprint of the containers' image digests,
// used to decide whether a status update is worth re-reporting. Container
// names cannot contain the separators, so the encoding is unambiguous.
func digestSignature(containers []Container) string {
	var b strings.Builder
	for _, c := range containers {
		b.WriteString(c.Name)
		b.WriteByte('=')
		b.WriteString(c.ImageDigest)
		b.WriteByte('\n')
	}
	return b.String()
}

// containerDigests maps container name to the image content digest reported in
// status, for the containers that have started. Init and regular container
// statuses are both consulted. Containers without a digest yet are simply
// absent from the map.
func containerDigests(pod *corev1.Pod) map[string]string {
	var digests map[string]string
	collect := func(statuses []corev1.ContainerStatus) {
		for _, s := range statuses {
			digest := parseImageDigest(s.ImageID)
			if digest == "" {
				continue
			}
			if digests == nil {
				digests = make(map[string]string)
			}
			digests[s.Name] = digest
		}
	}
	collect(pod.Status.InitContainerStatuses)
	collect(pod.Status.ContainerStatuses)
	return digests
}

// parseImageDigest extracts the content digest from a container status
// imageID. The kubelet reports imageID in a few shapes depending on the
// runtime: "registry/repo@sha256:…", the older
// "docker-pullable://registry/repo@sha256:…", or occasionally a bare
// "sha256:…" with no repository. The digest itself — everything from the
// algorithm prefix on — is what identifies the image content across registries
// and mirrors, so the registry prefix is discarded and only the digest kept.
// Returns "" when the imageID carries no digest (e.g. before the image is
// pulled, or a runtime that reports only a local reference).
func parseImageDigest(imageID string) string {
	if at := strings.LastIndex(imageID, "@"); at >= 0 {
		return imageID[at+1:]
	}
	// No repository prefix: accept a bare digest, reject a bare tag/reference.
	if strings.HasPrefix(imageID, "sha256:") {
		return imageID
	}
	return ""
}

// portsOf reduces a container's declared ports to the collected view: the
// spec fact only, with no interpretation of use.
func portsOf(c *corev1.Container) []ContainerPort {
	if len(c.Ports) == 0 {
		return nil
	}
	ports := make([]ContainerPort, 0, len(c.Ports))
	for _, p := range c.Ports {
		ports = append(ports, ContainerPort{
			Name:     p.Name,
			Port:     p.ContainerPort,
			Protocol: string(p.Protocol),
		})
	}
	return ports
}

// resourcesOf normalizes a container's declared requests and limits:
// CPU quantities to millicores, memory to bytes. Absent entries stay nil.
func resourcesOf(c *corev1.Container) Resources {
	var r Resources
	if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
		r.CPURequestMilli = ptr.To(q.MilliValue())
	}
	if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		r.CPULimitMilli = ptr.To(q.MilliValue())
	}
	if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
		r.MemoryRequestBytes = ptr.To(q.Value())
	}
	if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		r.MemoryLimitBytes = ptr.To(q.Value())
	}
	return r
}

// resolveWorkload walks the owner chain one controller hop at a time:
// Pod → ReplicaSet → Deployment (or Argo Rollout), Pod → Job → CronJob.
// Direct controllers (StatefulSet, DaemonSet, custom CRDs) are reported
// as-is; if an intermediate object is missing from the cache, the
// intermediate itself is reported rather than nothing.
func (w *PodWatcher) resolveWorkload(pod *corev1.Pod) WorkloadRef {
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return WorkloadRef{Kind: "none"}
	}
	switch owner.Kind {
	case "ReplicaSet":
		if rs, err := w.rsLister.ReplicaSets(pod.Namespace).Get(owner.Name); err == nil {
			if parent := metav1.GetControllerOf(rs); parent != nil {
				return WorkloadRef{Kind: parent.Kind, Name: parent.Name}
			}
		}
	case "Job":
		if job, err := w.jobLister.Jobs(pod.Namespace).Get(owner.Name); err == nil {
			if parent := metav1.GetControllerOf(job); parent != nil {
				return WorkloadRef{Kind: parent.Kind, Name: parent.Name}
			}
		}
	}
	return WorkloadRef{Kind: owner.Kind, Name: owner.Name}
}
