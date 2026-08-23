package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

var jobFinished = time.Date(2026, 8, 6, 10, 7, 0, 0, time.UTC)

func finishedJob(name string, condType batchv1.JobConditionType, reason string, owner *metav1.OwnerReference) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "analytics", Name: name, UID: types.UID("uid-" + name),
		},
		Spec: batchv1.JobSpec{
			Parallelism:  ptr.To(int32(1)),
			Completions:  ptr.To(int32(1)),
			BackoffLimit: ptr.To(int32(6)),
		},
		Status: batchv1.JobStatus{
			StartTime: &metav1.Time{Time: jobFinished.Add(-2 * time.Minute)},
			Succeeded: 1,
			Failed:    2,
			Conditions: []batchv1.JobCondition{{
				Type:               condType,
				Status:             corev1.ConditionTrue,
				Reason:             reason,
				LastTransitionTime: metav1.Time{Time: jobFinished},
			}},
		},
	}
	if condType == batchv1.JobComplete {
		job.Status.CompletionTime = &metav1.Time{Time: jobFinished}
	}
	if owner != nil {
		job.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return job
}

func collectRuns(t *testing.T, filter *Filter, objects ...runtime.Object) map[string]JobRun {
	t.Helper()
	clientset := fake.NewClientset(objects...)

	var mu sync.Mutex
	runs := map[string]JobRun{}
	watcher := NewPodWatcher(clientset, func(PodInfo) {})
	watcher.SetFilter(filter)
	watcher.OnJobFinished(func(r JobRun) {
		mu.Lock()
		runs[r.Name] = r
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		c := filter.Snapshot()
		if c.JobsObserved+c.JobsExcludedNamespaceFilter+c.JobsExcludedNamespaceAnnotation+
			c.JobsExcludedWorkloadAnnotation+c.JobsExcludedAnnotation > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Give the remaining Adds a moment to drain before reading.
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]JobRun, len(runs))
	for k, v := range runs {
		out[k] = v
	}
	return out
}

// A finished Job becomes a run with the cluster's own instants and outcome, and
// it is reported even though it was already terminal when the informer synced:
// unlike a restart counter, a Job carries its own time (ADR 0029 §3).
func TestFinishedJobIsReportedWithItsOwnInstants(t *testing.T) {
	cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "analytics", Name: "rollup"}}
	owner := controllerRef("CronJob", "rollup")
	runs := collectRuns(t, NewFilter(nil, nil),
		cron,
		finishedJob("rollup-1", batchv1.JobComplete, "", &owner),
		finishedJob("rollup-2", batchv1.JobFailed, "BackoffLimitExceeded", &owner),
	)

	ok, found := runs["rollup-1"]
	if !found {
		t.Fatalf("the succeeded run was not reported; got %v", keysOf(runs))
	}
	if ok.Result != JobSucceeded || ok.FailReason != "" {
		t.Errorf("run = (%q, %q), want (%q, \"\")", ok.Result, ok.FailReason, JobSucceeded)
	}
	if !ok.FinishedAt.Equal(jobFinished) {
		t.Errorf("finished_at = %s, want %s (the cluster's instant, not the observation's)",
			ok.FinishedAt, jobFinished)
	}
	if ok.Workload.Kind != "CronJob" || ok.Workload.Name != "rollup" {
		t.Errorf("workload = %+v, want the scheduling CronJob", ok.Workload)
	}
	if ok.Succeeded != 1 || ok.Failed != 2 {
		t.Errorf("pod counts = (%d, %d), want (1, 2); the retries are what cost resources",
			ok.Succeeded, ok.Failed)
	}

	failed, found := runs["rollup-2"]
	if !found {
		t.Fatalf("the failed run was not reported; got %v", keysOf(runs))
	}
	if failed.Result != JobFailed || failed.FailReason != "BackoffLimitExceeded" {
		t.Errorf("failed run = (%q, %q), want (%q, %q)",
			failed.Result, failed.FailReason, JobFailed, "BackoffLimitExceeded")
	}
}

// A running Job is not a run.
func TestUnfinishedJobIsNotReported(t *testing.T) {
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "analytics", Name: "rollup-live", UID: "uid-live",
	}}
	filter := NewFilter(nil, nil)
	runs := collectRuns(t, filter, running)
	if len(runs) != 0 {
		t.Errorf("reported %v for a Job with no terminal condition", keysOf(runs))
	}
}

// The opt-out written the pre-ADR-0028 way — in the Job's pod template — is what
// excluded this run's pods. If job_runs ignored it the agent would ship facts
// about a workload whose pods it refuses to measure (ADR 0029 §4).
func TestPodTemplateOptOutExcludesTheRun(t *testing.T) {
	job := finishedJob("legacy-optout", batchv1.JobComplete, "", nil)
	job.Spec.Template.Annotations = map[string]string{CollectAnnotation: "false"}

	filter := NewFilter(nil, nil)
	runs := collectRuns(t, filter, job)
	if len(runs) != 0 {
		t.Errorf("reported %v for a run whose pod template opted out", keysOf(runs))
	}
	if got := filter.Snapshot(); got.JobsExcludedAnnotation != 1 {
		t.Errorf("jobs_excluded_annotation = %d, want 1", got.JobsExcludedAnnotation)
	}
}

// And the opt-out on the scheduling CronJob, which is ADR 0028's step applied to
// the Job path.
func TestCronJobOptOutExcludesItsRuns(t *testing.T) {
	cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{
		Namespace: "analytics", Name: "rollup",
		Annotations: map[string]string{CollectAnnotation: "false"},
	}}
	owner := controllerRef("CronJob", "rollup")

	filter := NewFilter(nil, nil)
	runs := collectRuns(t, filter, cron, finishedJob("rollup-1", batchv1.JobComplete, "", &owner))
	if len(runs) != 0 {
		t.Errorf("reported %v for a run of an opted-out CronJob", keysOf(runs))
	}
	if got := filter.Snapshot(); got.JobsExcludedWorkloadAnnotation != 1 {
		t.Errorf("jobs_excluded_workload_annotation = %d, want 1", got.JobsExcludedWorkloadAnnotation)
	}
}

func keysOf(runs map[string]JobRun) []string {
	out := make([]string, 0, len(runs))
	for k := range runs {
		out = append(out, k)
	}
	return out
}
