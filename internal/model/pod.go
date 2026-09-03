package model

// Resources is the declared resource envelope of one container, normalized
// to resource units: CPU in millicores, memory in bytes. A nil field means
// the corresponding request or limit is not set — a meaningful fact in
// itself, distinct from zero.
type Resources struct {
	CPURequestMilli    *int64 `json:"cpu_request_milli,omitempty"`
	CPULimitMilli      *int64 `json:"cpu_limit_milli,omitempty"`
	MemoryRequestBytes *int64 `json:"memory_request_bytes,omitempty"`
	MemoryLimitBytes   *int64 `json:"memory_limit_bytes,omitempty"`
}

// ContainerPort is a declared port from the pod spec — the fact that a
// container announces it, and nothing about whether it is ever used. Declared
// ports are how the controller locates pprof endpoints without blind scans
// (docs/security.md §4). Name and Protocol are omitted when unset.
type ContainerPort struct {
	Name     string `json:"name,omitempty"`
	Port     int32  `json:"port"`
	Protocol string `json:"protocol,omitempty"`
}

// Probe is one of a container's probes, reduced to its schedule and the kind of
// check it makes. What it checks — the command, the HTTP path, the headers — is
// removed before the object is cached, so it is not here to be read
// (ADR 0048 §1).
//
// The numbers are the API server's defaulted ones, which are the numbers the
// kubelet will use: an unset `periodSeconds` arrives as 10, not as zero.
type Probe struct {
	// Kind is exec, httpGet, tcpSocket or grpc. Empty means the probe declares
	// no handler, which the API server rejects — so it is a shape, not a state.
	Kind                string `json:"kind,omitempty"`
	InitialDelaySeconds int32  `json:"initial_delay_seconds,omitempty"`
	PeriodSeconds       int32  `json:"period_seconds,omitempty"`
	TimeoutSeconds      int32  `json:"timeout_seconds,omitempty"`
	FailureThreshold    int32  `json:"failure_threshold,omitempty"`
	SuccessThreshold    int32  `json:"success_threshold,omitempty"`
}

// Probes are a container's three probes. Each is absent when the container
// declares none, which is the state most probe findings are about.
type Probes struct {
	Liveness  *Probe `json:"liveness,omitempty"`
	Readiness *Probe `json:"readiness,omitempty"`
	Startup   *Probe `json:"startup,omitempty"`
}

// Container is the collected view of a container: name, image, the image
// digest once the container has started, declared resources, declared ports,
// probe schedules, and the runtime knobs named in ADR 0047. Args and command
// are never read, and neither is any other environment variable (filter early).
type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// ImageDigest is the content digest (e.g. "sha256:…") the kubelet reports
	// for the running image. It is empty until the container starts, because
	// the runtime only knows it after pulling the image — see describe.
	ImageDigest string          `json:"image_digest,omitempty"`
	Init        bool            `json:"init,omitempty"`
	Resources   Resources       `json:"resources"`
	Ports       []ContainerPort `json:"ports,omitempty"`
	// Probes are the container's probe schedules. A liveness probe is the one
	// piece of a spec that can restart a healthy container on a timer, and
	// nothing else the agent collects says it exists (ADR 0048 §1).
	Probes Probes `json:"probes,omitzero"`
	// RuntimeEnv holds the Go runtime knobs from the container's environment,
	// and only those: the names are a closed list, and a variable whose value
	// comes from a Secret or ConfigMap is not read (ADR 0047). A knob set from
	// the container's own limits carries the field path it derives from rather
	// than a value, because the value does not exist until the kubelet resolves
	// it.
	RuntimeEnv map[string]string `json:"runtime_env,omitempty"`
}

// PodInfo is the collected view of one pod.
type PodInfo struct {
	Namespace string
	Name      string
	Node      string
	Phase     string
	// Unscheduled is why the pod is not on a node yet, "" once it is scheduled.
	// It is the reason behind the shortfall the replica breakdown already shows
	// (ADR 0012 §5): the count was always visible, the cause was not.
	Unscheduled string
	QOSClass    string
	Workload    WorkloadRef
	// Placement is what the spec says about where this pod may run. It is a
	// pod fact, not a container one, and it is what workload metadata and node
	// metadata together cannot answer: why a workload cannot be moved.
	Placement Placement
	// Claims are the PersistentVolumeClaim names this pod mounts, and the only
	// part of `spec.volumes` the agent reads (ADR 0032, amending ADR 0031). A
	// bound claim on a zonal volume pins the pod to that zone, which no field
	// of the placement block above states.
	Claims     []string
	Containers []Container
}
