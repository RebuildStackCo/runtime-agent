package model

// WorkloadPolicy is everything outside a workload's own spec that bounds it:
// what may disrupt it, what scales it, what storage holds it in place, and what
// sends it traffic.
type WorkloadPolicy struct {
	Namespace string      `json:"namespace"`
	Workload  WorkloadRef `json:"workload"`
	// Budgets is plural because selectors overlap: two budgets may cover one
	// workload, and the binding one is the stricter. Which that is depends on
	// replica counts at a moment, so the agent reports both and decides
	// nothing (ADR 0004).
	Budgets     []DisruptionBudget `json:"budgets,omitempty"`
	Autoscalers []Autoscaler       `json:"autoscalers,omitempty"`
	Claims      []VolumeClaim      `json:"claims,omitempty"`
	// Services is plural for the same reason Budgets is: selectors overlap, and
	// one workload is routinely reachable through several.
	Services []ServiceExposure `json:"services,omitempty"`
}

// ClusterPolicy is the policy configuration of the cluster as a whole: what each
// collected namespace imposes, and the two cluster-scoped catalogs a workload's
// own fields point into by name.
type ClusterPolicy struct {
	Namespaces      []NamespacePolicy   `json:"namespaces,omitempty"`
	PriorityClasses []PriorityClassInfo `json:"priority_classes,omitempty"`
	StorageClasses  []StorageClassInfo  `json:"storage_classes,omitempty"`
}

// NamespacePolicy is the policy a namespace imposes on everything inside it.
type NamespacePolicy struct {
	Namespace   string              `json:"namespace"`
	LimitRanges []LimitRangeInfo    `json:"limit_ranges,omitempty"`
	Quotas      []ResourceQuotaInfo `json:"quotas,omitempty"`
}

// Autoscaler is a HorizontalPodAutoscaler targeting a workload.
//
// It is the one object in this payload that prevents a wrong conclusion rather
// than adding a new fact. Under a CPU-utilization target the target is a
// percentage *of the request*, so changing the request changes scaling behavior:
// a replica count read without the autoscaler beside it invites advice that
// would alter how the workload scales.
type Autoscaler struct {
	Name            string             `json:"name"`
	MinReplicas     *int32             `json:"min_replicas,omitempty"`
	MaxReplicas     int32              `json:"max_replicas"`
	CurrentReplicas int32              `json:"current_replicas"`
	DesiredReplicas int32              `json:"desired_replicas"`
	Metrics         []AutoscalerMetric `json:"metrics,omitempty"`
	// LimitedReason is the reason of the ScalingLimited condition, from
	// Kubernetes' own vocabulary — `TooManyReplicas` says the workload is
	// pinned at its ceiling. The adjacent free-text message is never read
	// (ADR 0020 §6).
	LimitedReason string `json:"limited_reason,omitempty"`
}

// AutoscalerMetric is one metric an autoscaler scales on, reduced to what it
// targets and at what value.
type AutoscalerMetric struct {
	// Type is the metric source in Kubernetes' vocabulary: Resource,
	// ContainerResource, Pods, Object or External.
	Type string `json:"type"`
	// Name is the resource for a resource metric (`cpu`, `memory`) or the
	// metric's own name otherwise. An external metric's name is written by
	// whoever configured it, so it is bounded like the placement strings.
	Name string `json:"name"`
	// TargetType is Utilization, AverageValue or Value; TargetValue is the
	// threshold as written. A utilization target is a percentage of the
	// request, which is why the pair must travel together.
	TargetType  string `json:"target_type,omitempty"`
	TargetValue string `json:"target_value,omitempty"`
}

// DisruptionBudget is a PodDisruptionBudget covering a workload, reduced to what
// it permits and what it currently permits.
//
// It answers a question no other payload can: whether a node can be emptied at
// all. A budget allowing zero disruptions does not make consolidation expensive,
// it makes it impossible without someone changing the budget — and that is a
// different finding from "this node is too large".
type DisruptionBudget struct {
	Name string `json:"name"`
	// MinAvailable and MaxUnavailable are kept as written, because a budget may
	// state either as a count or as a percentage and the two are not
	// interchangeable when replicas change. Exactly one of them is set on any
	// real budget.
	MinAvailable   string `json:"min_available,omitempty"`
	MaxUnavailable string `json:"max_unavailable,omitempty"`
	// DisruptionsAllowed is the live answer: how many pods may be evicted right
	// now. Zero means a drain blocks today, whatever the declaration permits in
	// principle.
	DisruptionsAllowed int32 `json:"disruptions_allowed"`
	CurrentHealthy     int32 `json:"current_healthy"`
	DesiredHealthy     int32 `json:"desired_healthy"`
	ExpectedPods       int32 `json:"expected_pods"`
}

// EndpointZones is where one Service's ready endpoints of one address family
// sit, and how many of them the cluster gave a routing hint to.
//
// No endpoint is identified. The counts are a property of the Service's
// routing, the way node totals are a property of the node, and the addresses
// they are counted from never enter the agent's cache (ADR 0051 §1).
type EndpointZones struct {
	// AddressType is IPv4, IPv6 or FQDN, as the slice declares it.
	AddressType string `json:"address_type"`
	// Ready counts endpoints whose ready condition is true. An unset condition
	// counts as ready, which is what the API tells a consumer that does not
	// understand it to assume.
	Ready int `json:"ready"`
	// Zones counts those ready endpoints by zone.
	Zones map[string]int `json:"zones,omitempty"`
	// Unzoned counts ready endpoints carrying no zone at all — a node without
	// the topology label, not a zone whose name is empty.
	Unzoned int `json:"unzoned,omitempty"`
	// Hinted counts ready endpoints the EndpointSlice controller gave
	// `hints.forZones`. Zero against a set TrafficDistribution is the whole
	// finding: the routing was asked for and the cluster declined to arrange
	// it, silently (ADR 0051).
	Hinted int `json:"hinted"`
}

// LimitRangeInfo is one LimitRange object.
type LimitRangeInfo struct {
	Name  string           `json:"name"`
	Items []LimitRangeItem `json:"items,omitempty"`
}

// LimitRangeItem is one entry of a LimitRange: what a namespace injects into a
// container that declares nothing, and the band it must stay inside.
//
// It changes the meaning of numbers already shipped. A request of 100m may be
// what a team chose or what the namespace supplied on their behalf, and
// `workload_metadata` alone cannot tell those apart.
type LimitRangeItem struct {
	// Type is Container, Pod or PersistentVolumeClaim.
	Type string `json:"type"`
	// DefaultRequest and Default are what a container that declares nothing
	// receives — the request and the limit respectively, in Kubernetes' own
	// naming.
	DefaultRequest ResourceAmounts `json:"default_request,omitzero"`
	Default        ResourceAmounts `json:"default,omitzero"`
	Min            ResourceAmounts `json:"min,omitzero"`
	Max            ResourceAmounts `json:"max,omitzero"`
}

// PriorityClassInfo is the value and preemption behavior behind a
// `priorityClassName` already collected in the placement block (ADR 0031), and
// the explanation for preemptions already reported in `pod_disruptions`.
type PriorityClassInfo struct {
	Name             string `json:"name"`
	Value            int32  `json:"value"`
	GlobalDefault    bool   `json:"global_default,omitempty"`
	PreemptionPolicy string `json:"preemption_policy,omitempty"`
}

// ResourceAmounts is the cpu/memory/storage triple a LimitRange entry may carry.
// Absent means the entry sets nothing for that resource, which is distinct from
// setting zero.
type ResourceAmounts struct {
	CPUMilli     *int64 `json:"cpu_milli,omitempty"`
	MemoryBytes  *int64 `json:"memory_bytes,omitempty"`
	StorageBytes *int64 `json:"storage_bytes,omitempty"`
}

// ResourceQuotaInfo is one ResourceQuota: the ceiling a namespace sits under and
// how much of it is spent. It says why a workload cannot grow, and how much room
// remains if it should.
type ResourceQuotaInfo struct {
	Name string `json:"name"`
	// Hard and Used are kept keyed by Kubernetes' own resource names
	// (`requests.cpu`, `limits.memory`, `pods`, `count/deployments.apps`) with
	// quantities as written, because a quota's vocabulary is open-ended and
	// flattening it to cpu/memory would silently drop the rest.
	Hard map[string]string `json:"hard,omitempty"`
	Used map[string]string `json:"used,omitempty"`
	// Scopes narrow which objects the quota counts (`BestEffort`,
	// `NotTerminating`, …), so a quota read without them reads as broader than
	// it is.
	Scopes []string `json:"scopes,omitempty"`
}

// ServiceExposure is a Service whose selector matches this workload's admitted
// pods.
//
// It is what separates a workload whose replica count is an availability
// decision from one where it is a batch size: nothing else the agent collects
// says that traffic is routed to a workload at all (ADR 0048 §4). The selector
// is resolved through admitted pods, so a Service pointing only at excluded
// pods attaches to nothing and is never named (CLAUDE.md invariant 6).
type ServiceExposure struct {
	Name string `json:"name"`
	// Type is ClusterIP, NodePort, LoadBalancer or ExternalName.
	Type string `json:"type,omitempty"`
	// Headless is a ClusterIP of "None": clients resolve the pods themselves,
	// so there is no virtual address to fail over and a single replica is a
	// single address.
	Headless bool `json:"headless,omitempty"`
	// The two traffic policies say whether traffic may cross a node to reach a
	// replica. `Local` on either turns "the workload has replicas elsewhere"
	// into "the workload has no replica on this node", which is a different
	// answer to what a drain costs.
	InternalTrafficPolicy string `json:"internal_traffic_policy,omitempty"`
	ExternalTrafficPolicy string `json:"external_traffic_policy,omitempty"`
	// TrafficDistribution is `spec.trafficDistribution` verbatim — the
	// customer's request that traffic prefer a nearby endpoint.
	TrafficDistribution string `json:"traffic_distribution,omitempty"`
	// TopologyMode is the annotation that asked for the same thing before the
	// field existed, kept verbatim and bounded. A value outside Kubernetes' own
	// vocabulary is reported as written rather than normalized away: a
	// misspelled mode is a Service that asked for nothing while looking
	// configured, which is the finding (ADR 0051 §3).
	TopologyMode string `json:"topology_mode,omitempty"`
	// Endpoints is one entry per address family the Service publishes. They are
	// separate because a dual-stack Service lists the same pod in an IPv4 slice
	// and an IPv6 slice: summed, every zone doubles (ADR 0051 §2).
	Endpoints []EndpointZones `json:"endpoints,omitempty"`
}

// StorageClassInfo is one StorageClass, minus its parameters.
//
// `parameters` is the one field in this whole payload that carries provider
// configuration written by an operator — endpoints, resource groups, key
// identifiers — so it is not read. What is read is how the class behaves:
// `volumeBindingMode` decides whether a volume is bound before its pod is
// scheduled, which is exactly what turns a claim into a placement constraint.
type StorageClassInfo struct {
	Name                 string `json:"name"`
	Provisioner          string `json:"provisioner"`
	ReclaimPolicy        string `json:"reclaim_policy,omitempty"`
	VolumeBindingMode    string `json:"volume_binding_mode,omitempty"`
	AllowVolumeExpansion bool   `json:"allow_volume_expansion,omitempty"`
}

// VolumeClaim is one PersistentVolumeClaim a workload's pods mount.
//
// A bound claim on a zonal volume pins its pod to that zone for as long as the
// claim exists, which is a placement constraint no field of the pod spec states.
type VolumeClaim struct {
	Name           string   `json:"name"`
	StorageClass   string   `json:"storage_class,omitempty"`
	AccessModes    []string `json:"access_modes,omitempty"`
	RequestedBytes int64    `json:"requested_bytes,omitempty"`
	Phase          string   `json:"phase,omitempty"`
}
