package collector

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Scheduling reasons kept verbatim. They are the scheduler's own vocabulary for
// why a pod is not on a node, and they are not interchangeable: Unschedulable
// means nothing fits, SchedulingGated means the pod is deliberately held and is
// *not* a capacity problem, SchedulerError means the scheduler itself failed.
// Collapsing them would turn an intentional hold into a false capacity signal
// (ADR 0021).
const (
	reasonUnschedulable   = corev1.PodReasonUnschedulable
	reasonSchedulingGated = corev1.PodReasonSchedulingGated
	reasonSchedulerError  = corev1.PodReasonSchedulerError
)

// OtherReason is where a scheduling or disruption reason outside the known set
// is counted. The reasons come from the scheduler and the kubelet, never from
// user input, but the set is Kubernetes' to extend — and a payload whose keys an
// upstream release chooses has unbounded shape. Bucketing keeps the key set
// ours while still counting the pod.
const OtherReason = "other"

var knownSchedulingReasons = map[string]struct{}{
	reasonUnschedulable:   {},
	reasonSchedulingGated: {},
	reasonSchedulerError:  {},
}

// knownDisruptionReasons is the DisruptionTarget vocabulary. Every one of them
// is a capacity story: the kubelet reclaiming a node under pressure, the
// scheduler making room for higher priority, a taint manager draining, or the
// eviction API being called during a scale-down.
var knownDisruptionReasons = map[string]struct{}{
	corev1.PodReasonTerminationByKubelet:  {},
	corev1.PodReasonPreemptionByScheduler: {},
	"DeletionByTaintManager":              {},
	"DeletionByPodGC":                     {},
	"EvictionByEvictionAPI":               {},
}

// evictedPodReason is what the kubelet writes into status.reason when it evicts
// a pod under node pressure. It predates the DisruptionTarget condition and
// still appears alongside it.
const evictedPodReason = "Evicted"

// PodDisruption is one pod removed by the cluster rather than by its own
// workload: preempted to make room, evicted under node pressure, drained, or
// deleted by the eviction API. It is a journal fact — the object's status
// records that it happened (ADR 0021).
type PodDisruption struct {
	Namespace string
	Pod       string
	Workload  WorkloadRef
	// Node is where the pod was running when it was disrupted. It is the join to
	// node metadata, and for a node-pressure eviction it names the node that was
	// under pressure — the fact that makes the event actionable.
	Node string
	// Reason is the DisruptionTarget condition's reason, or "Evicted" when only
	// the older status.reason is present. Values come from Kubernetes.
	Reason string
	// DisruptedAt is the condition's transition time when Kubernetes recorded
	// one, and the observation instant otherwise. Unlike a restart, a disruption
	// *is* timestamped by the cluster, so an event already present when the
	// agent starts lands in the window where it actually happened.
	DisruptedAt time.Time
}

// OnPodDisruption registers fn to be called once per disrupted pod. Must be
// called before Run. fn is called from the informer goroutine and must not
// block.
func (w *PodWatcher) OnPodDisruption(fn func(PodDisruption)) {
	w.onDisruption = fn
}

// reportDisruptions reports a pod the cluster removed, exactly once.
//
// The DisruptionTarget condition is preferred because it carries both a precise
// reason and a transition time. status.phase/status.reason is the fallback: the
// kubelet sets it on a node-pressure eviction, and it survives on the object
// after the condition has served its purpose.
func (w *PodWatcher) reportDisruptions(pod *corev1.Pod) {
	if w.onDisruption == nil {
		return
	}
	reason, at, ok := disruptionOf(pod)
	if !ok {
		return
	}
	key := pod.Namespace + "/" + pod.Name
	w.mu.Lock()
	_, duplicate := w.reportedDisruptions[key]
	if !duplicate {
		w.reportedDisruptions[key] = struct{}{}
	}
	w.mu.Unlock()
	if duplicate {
		return
	}
	w.onDisruption(PodDisruption{
		Namespace:   pod.Namespace,
		Pod:         pod.Name,
		Workload:    w.resolveWorkload(pod),
		Node:        pod.Spec.NodeName,
		Reason:      bucketReason(reason, knownDisruptionReasons),
		DisruptedAt: at,
	})
}

// forgetDisruptions drops the dedup entry of a deleted pod. Losing the map on
// restart is harmless: a pod object still carrying the condition is reported
// once more, with the same timestamp, into the same window — a byte-identical
// rewrite of that window's payload.
func (w *PodWatcher) forgetDisruptions(pod *corev1.Pod) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.reportedDisruptions, pod.Namespace+"/"+pod.Name)
}

// disruptionOf reports whether the cluster removed this pod, with the reason and
// the moment it happened.
func disruptionOf(pod *corev1.Pod) (string, time.Time, bool) {
	if cond := podCondition(pod, corev1.DisruptionTarget); cond != nil && cond.Status == corev1.ConditionTrue {
		at := cond.LastTransitionTime.Time
		if at.IsZero() {
			at = time.Now()
		}
		return cond.Reason, at, true
	}
	if pod.Status.Phase == corev1.PodFailed && pod.Status.Reason == evictedPodReason {
		return evictedPodReason, time.Now(), true
	}
	return "", time.Time{}, false
}

// unscheduledReason returns why a pod is not on a node, or "" once it is
// scheduled. A pod with no PodScheduled condition at all has not been through
// the scheduler yet — a moment, not a state worth reporting.
func unscheduledReason(pod *corev1.Pod) string {
	cond := podCondition(pod, corev1.PodScheduled)
	if cond == nil || cond.Status != corev1.ConditionFalse {
		return ""
	}
	if cond.Reason == "" {
		return OtherReason
	}
	return bucketReason(cond.Reason, knownSchedulingReasons)
}

// bucketReason maps a reason to the key it is reported under: itself when known,
// OtherReason otherwise.
func bucketReason(reason string, known map[string]struct{}) string {
	if _, ok := known[reason]; ok {
		return reason
	}
	return OtherReason
}

func podCondition(pod *corev1.Pod, want corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == want {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}
