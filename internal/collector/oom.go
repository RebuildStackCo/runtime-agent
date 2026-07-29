package collector

import (
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// oomKilledReason is the reason the container runtime sets on a terminated
// container state when the kernel OOM killer ended it.
const oomKilledReason = "OOMKilled"

// OOMKill is one observed out-of-memory kill of a container, paired with the
// memory limit the container declared — the two facts that together make the
// "limit too low" story. MemoryLimitBytes is nil when no limit was set.
type OOMKill struct {
	Namespace        string      `json:"namespace"`
	Pod              string      `json:"pod"`
	Container        string      `json:"container"`
	Workload         WorkloadRef `json:"workload"`
	FinishedAt       time.Time   `json:"finished_at"`
	ExitCode         int32       `json:"exit_code"`
	RestartCount     int32       `json:"restart_count"`
	MemoryLimitBytes *int64      `json:"memory_limit_bytes,omitempty"`
}

// OnOOMKill registers fn to be called once per observed OOM kill. Must be
// called before Run. fn is called from the informer goroutine and must not
// block.
func (w *PodWatcher) OnOOMKill(fn func(OOMKill)) {
	w.onOOM = fn
}

// reportOOMKills scans a pod's container statuses for OOM-killed terminations
// and reports each one exactly once. A kill shows up in state.terminated
// while the container lies dead (restartPolicy Never) and moves to
// lastState.terminated after a restart; both carry the same finishedAt, so
// the dedup key covers the move.
func (w *PodWatcher) reportOOMKills(pod *corev1.Pod) {
	if w.onOOM == nil {
		return
	}
	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	for _, status := range statuses {
		for _, term := range []*corev1.ContainerStateTerminated{
			status.LastTerminationState.Terminated,
			status.State.Terminated,
		} {
			if term == nil || term.Reason != oomKilledReason {
				continue
			}
			key := oomKey(pod, status.Name, term)
			w.mu.Lock()
			_, dup := w.reportedOOMs[key]
			if !dup {
				w.reportedOOMs[key] = struct{}{}
			}
			w.mu.Unlock()
			if dup {
				continue
			}
			w.onOOM(OOMKill{
				Namespace:        pod.Namespace,
				Pod:              pod.Name,
				Container:        status.Name,
				Workload:         w.resolveWorkload(pod),
				FinishedAt:       term.FinishedAt.Time,
				ExitCode:         term.ExitCode,
				RestartCount:     status.RestartCount,
				MemoryLimitBytes: memoryLimitOf(pod, status.Name),
			})
		}
	}
}

// forgetOOMKills drops the dedup entries of a deleted pod so the map does not
// grow for the lifetime of the agent. Losing the whole map on restart is
// harmless: the same OOM gets reported again, nothing is missed.
func (w *PodWatcher) forgetOOMKills(pod *corev1.Pod) {
	prefix := pod.Namespace + "/" + pod.Name + "/"
	w.mu.Lock()
	defer w.mu.Unlock()
	for key := range w.reportedOOMs {
		if strings.HasPrefix(key, prefix) {
			delete(w.reportedOOMs, key)
		}
	}
}

// oomKey identifies one termination: container names cannot contain "/", and
// finishedAt distinguishes repeated kills of the same container.
func oomKey(pod *corev1.Pod, container string, term *corev1.ContainerStateTerminated) string {
	return pod.Namespace + "/" + pod.Name + "/" + container + "@" + term.FinishedAt.UTC().Format(time.RFC3339Nano)
}

// memoryLimitOf returns the declared memory limit of the named container,
// nil when the container has none.
func memoryLimitOf(pod *corev1.Pod, container string) *int64 {
	for _, c := range pod.Spec.InitContainers {
		if c.Name == container {
			return resourcesOf(&c).MemoryLimitBytes
		}
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == container {
			return resourcesOf(&c).MemoryLimitBytes
		}
	}
	return nil
}
