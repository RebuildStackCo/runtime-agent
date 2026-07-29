// Package collector observes cluster workloads. It reads and reduces; it
// never mutates cluster state.
package collector

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	batchlisters "k8s.io/client-go/listers/batch/v1"
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

// Container is the collected view of a container: name, image, and declared
// resources only. Env, args, and command are deliberately never read
// (filter early).
type Container struct {
	Name      string    `json:"name"`
	Image     string    `json:"image"`
	Init      bool      `json:"init,omitempty"`
	Resources Resources `json:"resources"`
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

	rsLister  appslisters.ReplicaSetLister
	jobLister batchlisters.JobLister

	mu           sync.Mutex
	reportedOOMs map[string]struct{}
}

// NewPodWatcher returns a watcher that calls onPod for every pod present at
// start and for every pod created afterwards. onPod is called from the
// informer goroutine and must not block.
func NewPodWatcher(clientset kubernetes.Interface, onPod func(PodInfo)) *PodWatcher {
	return &PodWatcher{
		clientset:    clientset,
		onPod:        onPod,
		reportedOOMs: make(map[string]struct{}),
	}
}

// Run blocks until ctx is canceled. ReplicaSet and Job caches are synced
// before pod events are delivered, so owner chains resolve from the first
// event on.
func (w *PodWatcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactory(w.clientset, 0)
	rs := factory.Apps().V1().ReplicaSets()
	jobs := factory.Batch().V1().Jobs()
	pods := factory.Core().V1().Pods()
	w.rsLister = rs.Lister()
	w.jobLister = jobs.Lister()

	// Informers must be instantiated before Start, or the factory won't
	// run them.
	podsInformer := pods.Informer()
	rsSynced, jobsSynced := rs.Informer().HasSynced, jobs.Informer().HasSynced

	factory.Start(ctx.Done())
	defer factory.Shutdown()

	if !cache.WaitForCacheSync(ctx.Done(), rsSynced, jobsSynced, podsInformer.HasSynced) {
		if ctx.Err() != nil {
			return nil // canceled during sync — a normal shutdown, not a failure
		}
		return fmt.Errorf("informer caches did not sync")
	}

	// Registered after sync: the handler replays every pod already in the
	// cache as an Add, then receives genuinely new pods as they appear.
	reg, err := podsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if pod, ok := obj.(*corev1.Pod); ok {
				w.onPod(w.describe(pod))
				w.reportOOMKills(pod)
			}
		},
		// OOM kills happen after a pod appears and arrive as status
		// updates; the pod itself is not re-reported.
		UpdateFunc: func(_, obj any) {
			if pod, ok := obj.(*corev1.Pod); ok {
				w.reportOOMKills(pod)
			}
		},
		DeleteFunc: func(obj any) {
			if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = unknown.Obj
			}
			if pod, ok := obj.(*corev1.Pod); ok {
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
	for _, c := range pod.Spec.InitContainers {
		info.Containers = append(info.Containers,
			Container{Name: c.Name, Image: c.Image, Init: true, Resources: resourcesOf(&c)})
	}
	for _, c := range pod.Spec.Containers {
		info.Containers = append(info.Containers,
			Container{Name: c.Name, Image: c.Image, Resources: resourcesOf(&c)})
	}
	return info
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
