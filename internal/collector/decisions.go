package collector

import (
	"sort"

	"k8s.io/apimachinery/pkg/labels"
)

// Decision is what the filter decided about one object, named. It is answered
// only inside the cluster and never enters a payload (ADR 0084).
type Decision struct {
	Kind      string             `json:"kind"`
	Namespace string             `json:"namespace,omitempty"`
	Name      string             `json:"name"`
	Collected bool               `json:"collected"`
	Profiled  bool               `json:"profiled"`
	Reason    ExclusionReason    `json:"reason,omitempty"`
	Workload  UnresolvedWorkload `json:"workload,omitempty"`
}

// Decisions re-asks the filter about every object it gates and returns the
// answers, named. Nothing is remembered to answer this: the caches hold every
// namespace, pod and Job whether or not it was admitted, so the list is a
// projection of what the informers already have (invariant 4).
//
// Counted nowhere. The counters describe the decisions the agent acted on, and
// a read of this list is not one of them.
func (w *PodWatcher) Decisions() []Decision {
	out := w.namespaceDecisions()
	out = append(out, w.podDecisions()...)
	out = append(out, w.jobDecisions()...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return out
}

// namespaceDecisions answers for the namespaces themselves. Their pods carry
// the same answer one by one, but a namespace with no pods in it would
// otherwise never show that the configuration reaches it.
func (w *PodWatcher) namespaceDecisions() []Decision {
	namespaces, err := w.nsLister.List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]Decision, 0, len(namespaces))
	for _, ns := range namespaces {
		collected, reason := w.filter.AdmitNamespace(ns)
		out = append(out, Decision{
			Kind:      "Namespace",
			Name:      ns.Name,
			Collected: collected,
			Profiled:  collected && ns.Annotations[ProfileAnnotation] != "false",
			Reason:    reason,
		})
	}
	return out
}

func (w *PodWatcher) podDecisions() []Decision {
	pods, err := w.podLister.List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]Decision, 0, len(pods))
	for _, pod := range pods {
		nsAnnotations := w.namespaceAnnotations(pod.Namespace)
		workload := w.workloadAnnotations(pod)
		collected, reason := w.filter.AdmitPod(pod, nsAnnotations, workload)
		out = append(out, Decision{
			Kind:      "Pod",
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Collected: collected,
			Profiled:  collected && w.filter.AdmitProfiling(pod, nsAnnotations, workload),
			Reason:    reason,
			Workload:  workload.Unresolved,
		})
	}
	return out
}

// jobDecisions answers for finished-Job facts, which are gated by a filter of
// their own (ADR 0028 §1). A Job's pods answer for the pods; this answers for
// `job_runs`, and the two can differ — the Job's pod template is a step only
// the Job's decision has.
func (w *PodWatcher) jobDecisions() []Decision {
	jobs, err := w.jobLister.List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]Decision, 0, len(jobs))
	for _, job := range jobs {
		workload := w.jobWorkloadAnnotations(job)
		collected, reason := w.filter.AdmitJob(job, w.namespaceAnnotations(job.Namespace), workload)
		out = append(out, Decision{
			Kind:      "Job",
			Namespace: job.Namespace,
			Name:      job.Name,
			Collected: collected,
			Reason:    reason,
			Workload:  workload.Unresolved,
		})
	}
	return out
}

// namespaceAnnotations reads a namespace's annotations from the cache, where a
// miss reads as none — the same reading the event path takes.
func (w *PodWatcher) namespaceAnnotations(namespace string) map[string]string {
	ns, err := w.nsLister.Get(namespace)
	if err != nil {
		return nil
	}
	return ns.Annotations
}
