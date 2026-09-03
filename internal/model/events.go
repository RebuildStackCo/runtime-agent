package model

import "time"

// ContainerRestart is one observed advance of a container's restart counter,
// paired with the termination that most recently caused one. It is the raw
// journal fact; the windowed aggregate is assembled downstream (ADR 0020).
//
// Restarts and Reason are of deliberately different quality. Restarts is exact —
// a counter delta counts every restart even if several happened between two
// status updates. Reason is a sample: a status carries only its most recent
// termination, so a burst with different reasons reports one of them.
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
