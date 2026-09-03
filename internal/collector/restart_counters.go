package collector

import (
	"sort"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// RestartCounters returns the current counter reading for every collected
// container that has ever restarted, sorted so the payload bytes are
// deterministic.
//
// A container that has never restarted contributes no record: the payload is a
// superseding snapshot, so absence says "zero". Scope runs through the admitted
// pod index like every other flush-time payload, so an excluded pod is
// unreachable here by construction rather than by a second check.
func (w *PodWatcher) RestartCounters() []model.RestartCounter {
	if w.podLister == nil {
		return nil
	}
	pods, err := w.podLister.List(labels.Everything())
	if err != nil {
		return nil
	}

	var out []model.RestartCounter
	for _, pod := range pods {
		_, workload, ok := w.LookupPod(pod.UID)
		if !ok {
			continue
		}
		statuses := make([]corev1.ContainerStatus, 0,
			len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
		statuses = append(statuses, pod.Status.InitContainerStatuses...)
		statuses = append(statuses, pod.Status.ContainerStatuses...)
		for _, status := range statuses {
			if status.RestartCount <= 0 {
				continue
			}
			base, seen := w.restartBaselineOf(restartKey(pod, status.Name))
			if !seen {
				// The pod is indexed but its counters have not been recorded
				// yet, which the informer does in the same handler a moment
				// earlier. Reporting now would have to guess how much of the
				// count the agent watched; the next flush knows.
				continue
			}
			out = append(out, model.RestartCounter{
				Namespace: pod.Namespace,
				Pod:       pod.Name,
				Container: status.Name,
				Workload:  workload,
				// Both values come from the one baseline read above, never one
				// from the baseline and one from the lister (ADR 0043): the
				// lister is the shared store, which client-go updates before
				// dispatching to the handler that maintains this baseline.
				// Taking both from one struct makes the ordering hold by
				// construction — a rebaseline sets the two together, and an
				// advance only ever raises last.
				Restarts:                  int64(base.last),
				RestartsBeforeObservation: int64(base.atFirstSight),
				ObservedSince:             base.firstSeen.UTC(),
				PodCreatedAt:              pod.CreationTimestamp.UTC(),
				ContainerStartedAt:        runningSince(status),
				LastTermination:           terminationOf(status),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Pod != b.Pod {
			return a.Pod < b.Pod
		}
		return a.Container < b.Container
	})
	return out
}

// restartBaselineOf reads one container's remembered counter under the lock the
// informer goroutine writes it with.
func (w *PodWatcher) restartBaselineOf(key string) (restartBaseline, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	base, ok := w.restartCounts[key]
	return base, ok
}

// runningSince is when the container's current incarnation started, nil when it
// is not running.
func runningSince(status corev1.ContainerStatus) *time.Time {
	if status.State.Running == nil || status.State.Running.StartedAt.IsZero() {
		return nil
	}
	started := status.State.Running.StartedAt.UTC()
	return &started
}

// terminationOf reduces the most recent termination to what may leave the
// cluster.
func terminationOf(status corev1.ContainerStatus) *model.RestartTermination {
	term := lastTerminationState(status)
	if term == nil {
		return nil
	}
	return &model.RestartTermination{
		Reason:     term.Reason,
		ExitCode:   term.ExitCode,
		FinishedAt: term.FinishedAt.UTC(),
	}
}
