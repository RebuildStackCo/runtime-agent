package collector

import (
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ContainerRestart is one observed advance of a container's restart counter,
// paired with the termination that most recently caused one. It is the raw
// journal fact; the windowed aggregate is assembled downstream (ADR 0020).
//
// Restarts and Reason are of deliberately different quality, and the split is
// the point. Restarts is exact: `restartCount` is a counter, so its delta since
// the previous observation counts every restart even if several happened
// between two status updates. Reason is a sample: a container status carries
// only its most recent termination, so a burst of restarts with different
// reasons reports one of them. Downstream the difference is a number, never a
// silent approximation.
type ContainerRestart struct {
	Namespace string
	Pod       string
	Container string
	Workload  WorkloadRef
	// ObservedAt is when the agent saw the counter advance, and is what places
	// the restarts in a window. The restarts themselves are not timestamped by
	// Kubernetes — only the most recent termination is — so this is the only
	// honest placement available.
	ObservedAt time.Time
	// Restarts is how far the counter advanced since the previous observation.
	Restarts int64
	// Reason is the termination reason behind the most recent restart, "" when
	// the status carried no terminated state. Values come from the kubelet and
	// the container runtime ("OOMKilled", "Error", "Completed", …), never from
	// user input.
	Reason string
	// ExitCode is that same termination's exit code, nil when unavailable.
	ExitCode *int32
}

// OnContainerRestart registers fn to be called for every observed advance of a
// container's restart counter. Must be called before Run. fn is called from the
// informer goroutine and must not block.
func (w *PodWatcher) OnContainerRestart(fn func(ContainerRestart)) {
	w.onRestart = fn
}

// reportRestarts compares each container's restart counter against the last
// value seen and reports the advance.
//
// A container seen for the first time is only baselined, never reported. Its
// counter may already stand at 40, but those restarts happened at moments
// Kubernetes does not record — placing them in the window that happens to be
// open when the agent starts would invent a history. This is the one place
// where the journal is deliberately incomplete, and it is bounded: it costs the
// restarts that happened before the agent watched the pod, once.
//
// A counter that does not advance reports nothing, and one that goes backwards
// rebaselines silently — the same rule the usage poller applies to every
// counter it reads (see usage.go, "Container restarted: rebaseline").
func (w *PodWatcher) reportRestarts(pod *corev1.Pod) {
	if w.onRestart == nil {
		return
	}
	observedAt := time.Now()
	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	for _, status := range statuses {
		key := restartKey(pod, status.Name)
		w.mu.Lock()
		previous, seen := w.restartCounts[key]
		w.restartCounts[key] = status.RestartCount
		w.mu.Unlock()
		if !seen || status.RestartCount <= previous {
			continue
		}
		reason, exitCode := lastTermination(status)
		w.onRestart(ContainerRestart{
			Namespace:  pod.Namespace,
			Pod:        pod.Name,
			Container:  status.Name,
			Workload:   w.resolveWorkload(pod),
			ObservedAt: observedAt,
			Restarts:   int64(status.RestartCount - previous),
			Reason:     reason,
			ExitCode:   exitCode,
		})
	}
}

// forgetRestarts drops the counter baselines of a deleted pod so the map does
// not grow for the lifetime of the agent. Losing them on restart is harmless in
// the same bounded way the first observation is: the counters are simply
// baselined again.
func (w *PodWatcher) forgetRestarts(pod *corev1.Pod) {
	prefix := pod.Namespace + "/" + pod.Name + "/"
	w.mu.Lock()
	defer w.mu.Unlock()
	for key := range w.restartCounts {
		if strings.HasPrefix(key, prefix) {
			delete(w.restartCounts, key)
		}
	}
}

// restartKey identifies one container of one pod. Container names cannot
// contain "/", so the concatenation is unambiguous.
func restartKey(pod *corev1.Pod, container string) string {
	return pod.Namespace + "/" + pod.Name + "/" + container
}

// lastTermination returns the reason and exit code of the termination behind
// the most recent restart. LastTerminationState is the one that describes it: a
// container in CrashLoopBackOff is currently *waiting*, and its previous death
// is what the counter counted. State.Terminated is the fallback for a container
// lying dead right now.
func lastTermination(status corev1.ContainerStatus) (string, *int32) {
	for _, term := range []*corev1.ContainerStateTerminated{
		status.LastTerminationState.Terminated,
		status.State.Terminated,
	} {
		if term == nil {
			continue
		}
		code := term.ExitCode
		return term.Reason, &code
	}
	return "", nil
}
