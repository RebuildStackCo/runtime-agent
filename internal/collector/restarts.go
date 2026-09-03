package collector

import (
	"strings"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	corev1 "k8s.io/api/core/v1"
)

// restartBaseline is what the agent remembers about one container's restart
// counter: the value each reported advance is measured against, and the value
// the counter already stood at when this process first saw the container.
//
// The two are different questions and only one of them changes. `last` tracks
// the counter so an advance can be turned into a delta; `atFirstSight` and
// `firstSeen` are fixed at the first observation and are what let the agent say
// how much of a container's history it did not watch (ADR 0034).
type restartBaseline struct {
	last         int32
	atFirstSight int32
	firstSeen    time.Time
}

// OnContainerRestart registers fn to be called for every observed advance of a
// container's restart counter. Must be called before Run. fn is called from the
// informer goroutine and must not block.
func (w *PodWatcher) OnContainerRestart(fn func(model.ContainerRestart)) {
	w.onRestart = fn
}

// reportRestarts compares each container's restart counter against the last
// value seen and reports the advance.
//
// A container seen for the first time is only baselined, never reported: those
// restarts happened at moments Kubernetes does not record, so putting them in
// the open window would invent a history. The number is kept as `atFirstSight`
// and shipped by `restart_counters` instead (ADR 0034). A counter that does not
// advance reports nothing; one that goes backwards rebaselines silently.
func (w *PodWatcher) reportRestarts(pod *corev1.Pod) {
	observedAt := time.Now()
	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	for _, status := range statuses {
		key := restartKey(pod, status.Name)
		w.mu.Lock()
		base, seen := w.restartCounts[key]
		// A counter below the baseline is a container the agent is meeting for
		// the first time under a name it has seen before — a recreated pod
		// keeping its name. Its history is its own, so the whole baseline is
		// taken again rather than only the delta reference.
		if !seen || status.RestartCount < base.last {
			w.restartCounts[key] = restartBaseline{
				last:         status.RestartCount,
				atFirstSight: status.RestartCount,
				firstSeen:    observedAt,
			}
			w.mu.Unlock()
			continue
		}
		previous := base.last
		base.last = status.RestartCount
		w.restartCounts[key] = base
		w.mu.Unlock()
		if status.RestartCount == previous || w.onRestart == nil {
			continue
		}
		reason, exitCode := lastTermination(status)
		w.onRestart(model.ContainerRestart{
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
	term := lastTerminationState(status)
	if term == nil {
		return "", nil
	}
	code := term.ExitCode
	return term.Reason, &code
}

// lastTerminationState returns the terminated state behind the most recent
// restart, or nil when the container has never died. It is the whole struct
// rather than a pair because `restart_counters` also reports when that
// termination finished — the one instant Kubernetes does record about a restart
// (ADR 0034).
func lastTerminationState(status corev1.ContainerStatus) *corev1.ContainerStateTerminated {
	for _, term := range []*corev1.ContainerStateTerminated{
		status.LastTerminationState.Terminated,
		status.State.Terminated,
	} {
		if term != nil {
			return term
		}
	}
	return nil
}
