package collector

import (
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// RestartCounter is the kubelet's restart counter for one container as it
// stands, together with what the agent knows about how much of it it watched.
//
// The counterpart of the restart journal, not a duplicate: the journal reports
// advances placed in the window they were observed in, this reports the total,
// including restarts that predate the agent. Neither derives from the other — a
// total cannot be spread over time, and a sum of windows cannot recover what
// preceded the first (ADR 0034).
type RestartCounter struct {
	Namespace string      `json:"namespace"`
	Pod       string      `json:"pod"`
	Container string      `json:"container"`
	Workload  WorkloadRef `json:"workload"`
	// Restarts is the kubelet's counter as the agent last observed it: every
	// restart of this container since the pod was created, as Kubernetes counts
	// them. "As last observed" rather than "now" is deliberate and is what
	// keeps it consistent with the field below — both are read from one record
	// of what the agent saw, so their difference is never negative (ADR 0043).
	// The lag is one informer dispatch; the payload flushes once a minute.
	Restarts int64 `json:"restarts"`
	// RestartsBeforeObservation is what the counter already stood at when this
	// agent process first saw the container. It is the number that is missing
	// from the windows, and on the day the agent is installed it is usually the
	// whole history.
	RestartsBeforeObservation int64 `json:"restarts_before_observation"`
	// ObservedSince is when that first sight happened. With PodCreatedAt it
	// bounds the interval RestartsBeforeObservation is spread over: somewhere
	// between the two, at instants Kubernetes does not record. An interval is
	// what the source supports, so an interval is what is reported.
	ObservedSince time.Time `json:"observed_since"`
	PodCreatedAt  time.Time `json:"pod_created_at"`
	// ContainerStartedAt is when the current incarnation started, absent when
	// the container is not running. It is how long the container has survived,
	// which is what separates forty restarts a fortnight ago from forty
	// restarts still happening.
	ContainerStartedAt *time.Time `json:"container_started_at,omitempty"`
	// LastTermination is the most recent death, the single restart Kubernetes
	// timestamps. Absent when the container has never died.
	LastTermination *RestartTermination `json:"last_termination,omitempty"`
}

// RestartTermination is the termination behind the most recent restart.
//
// The reason is carried under its own name rather than bucketed the way the
// journal's reason map is. Bucketing there protects a set of map *keys* the
// agent does not control (ADR 0020 §4); here the reason is a value, where
// cardinality costs nothing — the same treatment `unscheduled` reasons and an
// autoscaler's `limited_reason` already get. The adjacent free-text message is
// still never read (ADR 0020 §6).
type RestartTermination struct {
	Reason     string    `json:"reason,omitempty"`
	ExitCode   int32     `json:"exit_code"`
	FinishedAt time.Time `json:"finished_at"`
}

// RestartCounters returns the current counter reading for every collected
// container that has ever restarted, sorted so the payload bytes are
// deterministic.
//
// A container that has never restarted contributes no record: the payload is a
// superseding snapshot, so absence says "zero". Scope runs through the admitted
// pod index like every other flush-time payload, so an excluded pod is
// unreachable here by construction rather than by a second check.
func (w *PodWatcher) RestartCounters() []RestartCounter {
	if w.podLister == nil {
		return nil
	}
	pods, err := w.podLister.List(labels.Everything())
	if err != nil {
		return nil
	}

	var out []RestartCounter
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
			out = append(out, RestartCounter{
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
func terminationOf(status corev1.ContainerStatus) *RestartTermination {
	term := lastTerminationState(status)
	if term == nil {
		return nil
	}
	return &RestartTermination{
		Reason:     term.Reason,
		ExitCode:   term.ExitCode,
		FinishedAt: term.FinishedAt.UTC(),
	}
}
