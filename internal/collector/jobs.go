package collector

import (
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// JobRun is one finished execution of a Job: when it ran, how it ended, and the
// shape it was declared with. It is a journal fact — the object's status records
// that it happened (ADR 0029).
//
// It exists because usage rollups systematically misrepresent short-lived
// workloads: a Job running ninety seconds inside an hour-long window reports a
// tiny average, and `covered_nanoseconds` says coverage was low without saying
// why. This is the denominator.
type JobRun struct {
	Namespace string
	// Workload is the CronJob that scheduled this run, or the Job itself when
	// it has no controller. Many runs of one schedule therefore aggregate under
	// one workload, exactly as replicas of a Deployment do.
	Workload WorkloadRef
	// Name is this run's own object name. For a CronJob it is the generated
	// per-run name, which is what distinguishes two runs of one schedule.
	Name         string
	StartedAt    time.Time
	FinishedAt   time.Time
	Result       string
	FailReason   string
	Succeeded    int32
	Failed       int32
	Parallelism  *int32
	Completions  *int32
	BackoffLimit *int32
}

// Job outcomes. The vocabulary is Kubernetes' own condition types, not ours.
const (
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
)

// OnJobFinished registers fn to be called once per finished Job. Must be called
// before Run. fn is called from the informer goroutine and must not block.
func (w *PodWatcher) OnJobFinished(fn func(JobRun)) {
	w.onJobFinished = fn
}

// reportJobRun reports a Job that has reached a terminal state, exactly once.
//
// Unlike a container restart, a Job is timestamped by the cluster, so a run
// already finished when the agent starts lands in the window where it actually
// happened. That is why there is no baseline pass here and ADR 0020 §5 does not
// transfer: the reason restarts are baselined is that a restart counter carries
// no time, and this object does (ADR 0021 made the same distinction).
func (w *PodWatcher) reportJobRun(job *batchv1.Job) {
	if w.onJobFinished == nil {
		return
	}
	result, reason, finishedAt, ok := jobOutcomeOf(job)
	if !ok {
		return // still running, or never started
	}
	if !w.admitJob(job, true) {
		return
	}
	w.mu.Lock()
	_, duplicate := w.reportedJobs[job.UID]
	if !duplicate {
		w.reportedJobs[job.UID] = struct{}{}
	}
	w.mu.Unlock()
	if duplicate {
		return
	}

	run := JobRun{
		Namespace:    job.Namespace,
		Workload:     w.resolveJobWorkload(job),
		Name:         job.Name,
		FinishedAt:   finishedAt,
		Result:       result,
		FailReason:   reason,
		Succeeded:    job.Status.Succeeded,
		Failed:       job.Status.Failed,
		Parallelism:  job.Spec.Parallelism,
		Completions:  job.Spec.Completions,
		BackoffLimit: job.Spec.BackoffLimit,
	}
	if job.Status.StartTime != nil {
		run.StartedAt = job.Status.StartTime.UTC()
	}
	w.onJobFinished(run)
}

// forgetJobRun drops the dedup entry of a deleted Job. Losing the map on
// restart is harmless while the object still exists: the run is reported once
// more, with the same instants, into the same window — a byte-identical rewrite
// of that window's payload.
func (w *PodWatcher) forgetJobRun(job *batchv1.Job) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.reportedJobs, job.UID)
}

// resolveJobWorkload is the Job's controller when it has one, and the Job
// itself when it does not. It stops at one hop: a CronJob is the top of this
// chain by construction.
func (w *PodWatcher) resolveJobWorkload(job *batchv1.Job) WorkloadRef {
	if owner := metav1.GetControllerOf(job); owner != nil {
		return WorkloadRef{Kind: owner.Kind, Name: owner.Name}
	}
	return WorkloadRef{Kind: "Job", Name: job.Name}
}

// admitJob runs a Job through the filter: the workload step is its owning
// CronJob, the object step the Job's own annotations (where an annotation on a
// CronJob's `jobTemplate.metadata` lands), and the pod-template step is what
// excluded the run's pods.
//
// That last step keeps the payload honest — a customer who opted out the way
// security.md documented before ADR 0028 wrote the annotation into the pod
// template, and ignoring it would ship facts about a workload they refused.
func (w *PodWatcher) admitJob(job *batchv1.Job, count bool) bool {
	var nsAnnotations map[string]string
	if ns, err := w.nsLister.Get(job.Namespace); err == nil {
		nsAnnotations = ns.Annotations
	}
	workload := w.jobWorkloadAnnotations(job)
	allowed, reason := w.filter.AdmitJob(job, nsAnnotations, workload)
	if count {
		if allowed {
			w.filter.countJobObserved()
		} else {
			w.filter.countJobExcluded(reason)
		}
		if workload.Unresolved != "" {
			w.filter.countUnresolvedWorkload(workload.Unresolved)
		}
	}
	return allowed
}

// jobWorkloadAnnotations reads the annotations of the CronJob that scheduled
// this Job. A Job with no controller is its own workload and has nothing above
// it to consult, which is resolved rather than unresolved; a Job owned by
// something that is not a CronJob is a kind this agent does not read (ADR 0028).
func (w *PodWatcher) jobWorkloadAnnotations(job *batchv1.Job) WorkloadLookup {
	owner := metav1.GetControllerOf(job)
	if owner == nil {
		return WorkloadLookup{}
	}
	if owner.Kind != "CronJob" {
		return WorkloadLookup{Unresolved: WorkloadKindUnknown}
	}
	cron, err := w.cronLister.CronJobs(job.Namespace).Get(owner.Name)
	if err != nil {
		return WorkloadLookup{Unresolved: WorkloadNotCached}
	}
	return WorkloadLookup{Annotations: cron.Annotations}
}

// jobOutcomeOf reports whether the Job has finished, how, and when.
//
// Success is timestamped by `status.completionTime`, which Kubernetes sets only
// on success. A failure has no such field, so the terminal condition's
// transition time is the instant — the same fallback ADR 0021 uses for a
// disruption whose condition carries the only timestamp there is.
func jobOutcomeOf(job *batchv1.Job) (result, reason string, finishedAt time.Time, ok bool) {
	for _, cond := range job.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			at := job.Status.CompletionTime
			if at != nil && !at.IsZero() {
				return JobSucceeded, "", at.UTC(), true
			}
			return JobSucceeded, "", conditionTime(cond), true
		case batchv1.JobFailed:
			return JobFailed, cond.Reason, conditionTime(cond), true
		}
	}
	return "", "", time.Time{}, false
}

// conditionTime is the condition's transition time, falling back to now when
// the API server recorded none. A zero instant would file the run in the epoch
// window, which is worse than filing it slightly late.
func conditionTime(cond batchv1.JobCondition) time.Time {
	if cond.LastTransitionTime.IsZero() {
		return time.Now().UTC()
	}
	return cond.LastTransitionTime.UTC()
}
