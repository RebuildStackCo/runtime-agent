package collector

import (
	"sort"
	"strconv"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
)

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
}

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

// ResourceAmounts is the cpu/memory/storage triple a LimitRange entry may carry.
// Absent means the entry sets nothing for that resource, which is distinct from
// setting zero.
type ResourceAmounts struct {
	CPUMilli     *int64 `json:"cpu_milli,omitempty"`
	MemoryBytes  *int64 `json:"memory_bytes,omitempty"`
	StorageBytes *int64 `json:"storage_bytes,omitempty"`
}

func (r ResourceAmounts) empty() bool {
	return r.CPUMilli == nil && r.MemoryBytes == nil && r.StorageBytes == nil
}

// LimitRangeInfo is one LimitRange object.
type LimitRangeInfo struct {
	Name  string           `json:"name"`
	Items []LimitRangeItem `json:"items,omitempty"`
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

// NamespacePolicy is the policy a namespace imposes on everything inside it.
type NamespacePolicy struct {
	Namespace   string              `json:"namespace"`
	LimitRanges []LimitRangeInfo    `json:"limit_ranges,omitempty"`
	Quotas      []ResourceQuotaInfo `json:"quotas,omitempty"`
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

// ClusterPolicy is the policy configuration of the cluster as a whole: what each
// collected namespace imposes, and the two cluster-scoped catalogs a workload's
// own fields point into by name.
type ClusterPolicy struct {
	Namespaces      []NamespacePolicy   `json:"namespaces,omitempty"`
	PriorityClasses []PriorityClassInfo `json:"priority_classes,omitempty"`
	StorageClasses  []StorageClassInfo  `json:"storage_classes,omitempty"`
}

// WorkloadPolicies returns one record per workload that has admitted pods.
//
// The scope is inherited from the admitted pod index rather than decided again,
// the same construction ADR 0030 used for revisions: a workload excluded by any
// filter has no admitted pods and therefore cannot appear here, and there is no
// second admission path to keep in step.
func (w *PodWatcher) WorkloadPolicies() ([]WorkloadPolicy, []string) {
	unavailable := w.unavailablePolicySources(workloadPolicySources...)
	workloads, claimsByWorkload := w.collectedWorkloads()
	if len(workloads) == 0 {
		return nil, unavailable
	}

	byKey := make(map[workloadKey]*WorkloadPolicy, len(workloads))
	for key, ref := range workloads {
		byKey[key] = &WorkloadPolicy{Namespace: key.namespace, Workload: ref}
	}

	w.attachBudgets(byKey)
	w.attachAutoscalers(byKey)
	w.attachClaims(byKey, claimsByWorkload)
	w.attachServices(byKey)

	out := make([]WorkloadPolicy, 0, len(byKey))
	for _, p := range byKey {
		if len(p.Budgets) == 0 && len(p.Autoscalers) == 0 && len(p.Claims) == 0 && len(p.Services) == 0 {
			// A workload nothing constrains contributes no record. Silence here
			// is a fact — the payload is a snapshot, so an absent workload is
			// one with no policy attached, not one that went unobserved.
			continue
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Workload.Kind != out[j].Workload.Kind {
			return out[i].Workload.Kind < out[j].Workload.Kind
		}
		return out[i].Workload.Name < out[j].Workload.Name
	})
	return out, unavailable
}

// The caches each policy payload reads, listed per payload rather than pooled
// because each must be readable on its own: the two have distinct natural keys,
// so one can arrive without the other (ADR 0033).
//
// The storage-class catalog is absent from the workload list: a claim's
// `storage_class` is a name read from the PVC, and what that name means is a
// cluster-policy question.
var (
	workloadPolicySources = []string{
		"services", "pod_disruption_budgets", "horizontal_pod_autoscalers",
		"persistent_volume_claims",
	}
	clusterPolicySources = []string{
		"limit_ranges", "resource_quotas", "priority_classes", "storage_classes",
	}
)

// collectedWorkloads reads the admitted pod index once, returning every workload
// with admitted pods and the claim names those pods mount.
func (w *PodWatcher) collectedWorkloads() (map[workloadKey]WorkloadRef, map[workloadKey][]string) {
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()

	workloads := make(map[workloadKey]WorkloadRef)
	claims := make(map[workloadKey][]string)
	seen := make(map[workloadKey]map[string]bool)
	for _, entry := range w.index {
		key := workloadKey{namespace: entry.namespace, name: entry.workload.Name}
		workloads[key] = entry.workload
		for _, claim := range entry.info.Claims {
			if seen[key] == nil {
				seen[key] = make(map[string]bool)
			}
			if seen[key][claim] {
				continue
			}
			seen[key][claim] = true
			claims[key] = append(claims[key], claim)
		}
	}
	return workloads, claims
}

// attachBudgets matches every PodDisruptionBudget to the workloads it covers.
//
// A budget names pods by label selector, not by workload, so the mapping runs
// through pods — and only through admitted ones. A budget whose pods are all
// excluded therefore attaches to nothing and is never named, which keeps an
// excluded workload's identity out of the payload by construction (CLAUDE.md
// invariant 6).
func (w *PodWatcher) attachBudgets(byKey map[workloadKey]*WorkloadPolicy) {
	if w.pdbLister == nil {
		return
	}
	budgets, err := w.pdbLister.List(labels.Everything())
	if err != nil {
		return
	}
	for _, pdb := range budgets {
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil || selector.Empty() {
			continue
		}
		reduced := describeBudget(pdb)
		for _, key := range w.workloadsSelecting(pdb.Namespace, selector) {
			if p, ok := byKey[key]; ok {
				p.Budgets = append(p.Budgets, reduced)
			}
		}
	}
	for _, p := range byKey {
		sort.Slice(p.Budgets, func(i, j int) bool { return p.Budgets[i].Name < p.Budgets[j].Name })
	}
}

// attachServices matches every Service to the workloads its selector covers,
// through the same admitted-pod resolution a budget uses.
//
// A Service with no selector is skipped: an ExternalName, or one whose
// EndpointSlices are written by hand, points at something this agent cannot see
// from a label.
func (w *PodWatcher) attachServices(byKey map[workloadKey]*WorkloadPolicy) {
	if w.svcLister == nil {
		return
	}
	services, err := w.svcLister.List(labels.Everything())
	if err != nil {
		return
	}
	for _, svc := range services {
		if len(svc.Spec.Selector) == 0 {
			continue
		}
		reduced := describeService(svc)
		for _, key := range w.workloadsSelecting(svc.Namespace, labels.SelectorFromSet(svc.Spec.Selector)) {
			if p, ok := byKey[key]; ok {
				p.Services = append(p.Services, reduced)
			}
		}
	}
	for _, p := range byKey {
		sort.Slice(p.Services, func(i, j int) bool { return p.Services[i].Name < p.Services[j].Name })
	}
}

// describeService reduces a Service to how it routes. Its addresses are not
// read: a ClusterIP is cluster-internal and a load balancer's is the customer's
// public address, and no finding needs either.
func describeService(svc *corev1.Service) ServiceExposure {
	return ServiceExposure{
		Name:                  svc.Name,
		Type:                  string(svc.Spec.Type),
		Headless:              svc.Spec.ClusterIP == corev1.ClusterIPNone,
		InternalTrafficPolicy: internalTrafficPolicy(svc),
		ExternalTrafficPolicy: string(svc.Spec.ExternalTrafficPolicy),
	}
}

func internalTrafficPolicy(svc *corev1.Service) string {
	if svc.Spec.InternalTrafficPolicy == nil {
		return ""
	}
	return string(*svc.Spec.InternalTrafficPolicy)
}

// workloadsSelecting resolves a label selector to the workloads of the admitted
// pods it matches, deduplicated.
func (w *PodWatcher) workloadsSelecting(namespace string, selector labels.Selector) []workloadKey {
	if w.podLister == nil {
		return nil
	}
	pods, err := w.podLister.Pods(namespace).List(selector)
	if err != nil {
		return nil
	}
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()
	seen := make(map[workloadKey]bool)
	var out []workloadKey
	for _, pod := range pods {
		uid, ok := w.nameIndex[namespace+"/"+pod.Name]
		if !ok {
			continue
		}
		entry, ok := w.index[uid]
		if !ok {
			continue
		}
		key := workloadKey{namespace: entry.namespace, name: entry.workload.Name}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// attachAutoscalers matches every HorizontalPodAutoscaler to its target. Unlike
// a budget, an autoscaler names its workload directly in `scaleTargetRef`, so
// the join is exact rather than through labels.
func (w *PodWatcher) attachAutoscalers(byKey map[workloadKey]*WorkloadPolicy) {
	if w.hpaLister == nil {
		return
	}
	scalers, err := w.hpaLister.List(labels.Everything())
	if err != nil {
		return
	}
	for _, hpa := range scalers {
		key := workloadKey{namespace: hpa.Namespace, name: hpa.Spec.ScaleTargetRef.Name}
		p, ok := byKey[key]
		if !ok || p.Workload.Kind != hpa.Spec.ScaleTargetRef.Kind {
			continue
		}
		p.Autoscalers = append(p.Autoscalers, describeAutoscaler(hpa))
	}
	for _, p := range byKey {
		sort.Slice(p.Autoscalers, func(i, j int) bool { return p.Autoscalers[i].Name < p.Autoscalers[j].Name })
	}
}

func (w *PodWatcher) attachClaims(byKey map[workloadKey]*WorkloadPolicy, claimsByWorkload map[workloadKey][]string) {
	if w.pvcLister == nil {
		return
	}
	for key, names := range claimsByWorkload {
		p, ok := byKey[key]
		if !ok {
			continue
		}
		for _, name := range names {
			claim, err := w.pvcLister.PersistentVolumeClaims(key.namespace).Get(name)
			if err != nil {
				continue
			}
			p.Claims = append(p.Claims, describeClaim(claim))
		}
		sort.Slice(p.Claims, func(i, j int) bool { return p.Claims[i].Name < p.Claims[j].Name })
	}
}

// ClusterPolicy returns the namespace policy of every collected namespace and
// the two cluster-scoped catalogs.
//
// Namespaces are those with admitted pods, so an excluded namespace is never
// named. The catalogs are not filtered: a PriorityClass and a StorageClass are
// cluster infrastructure rather than a customer workload, and the agent already
// reports the whole node fleet on the same reasoning (ADR 0012). Their names are
// what the placement block and the claims above point into.
func (w *PodWatcher) ClusterPolicy() (ClusterPolicy, []string) {
	unavailable := w.unavailablePolicySources(clusterPolicySources...)
	var out ClusterPolicy
	for _, ns := range w.collectedNamespaces() {
		policy := NamespacePolicy{Namespace: ns}
		policy.LimitRanges = w.limitRangesIn(ns)
		policy.Quotas = w.quotasIn(ns)
		if len(policy.LimitRanges) == 0 && len(policy.Quotas) == 0 {
			continue
		}
		out.Namespaces = append(out.Namespaces, policy)
	}
	out.PriorityClasses = w.priorityClasses()
	out.StorageClasses = w.storageClasses()
	return out, unavailable
}

func (w *PodWatcher) collectedNamespaces() []string {
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()
	seen := make(map[string]bool)
	var out []string
	for _, entry := range w.index {
		if seen[entry.namespace] {
			continue
		}
		seen[entry.namespace] = true
		out = append(out, entry.namespace)
	}
	sort.Strings(out)
	return out
}

func (w *PodWatcher) limitRangesIn(namespace string) []LimitRangeInfo {
	if w.limitRangeLister == nil {
		return nil
	}
	ranges, err := w.limitRangeLister.LimitRanges(namespace).List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]LimitRangeInfo, 0, len(ranges))
	for _, lr := range ranges {
		out = append(out, describeLimitRange(lr))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (w *PodWatcher) quotasIn(namespace string) []ResourceQuotaInfo {
	if w.quotaLister == nil {
		return nil
	}
	quotas, err := w.quotaLister.ResourceQuotas(namespace).List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]ResourceQuotaInfo, 0, len(quotas))
	for _, q := range quotas {
		out = append(out, describeQuota(q))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (w *PodWatcher) priorityClasses() []PriorityClassInfo {
	if w.priorityLister == nil {
		return nil
	}
	classes, err := w.priorityLister.List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]PriorityClassInfo, 0, len(classes))
	for _, c := range classes {
		if !fits(c.Name) {
			continue
		}
		info := PriorityClassInfo{
			Name:          c.Name,
			Value:         c.Value,
			GlobalDefault: c.GlobalDefault,
		}
		if c.PreemptionPolicy != nil {
			info.PreemptionPolicy = string(*c.PreemptionPolicy)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (w *PodWatcher) storageClasses() []StorageClassInfo {
	if w.storageClassLister == nil {
		return nil
	}
	classes, err := w.storageClassLister.List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]StorageClassInfo, 0, len(classes))
	for _, c := range classes {
		if !fits(c.Name) || !fits(c.Provisioner) {
			continue
		}
		info := StorageClassInfo{
			Name:        c.Name,
			Provisioner: c.Provisioner,
		}
		if c.ReclaimPolicy != nil {
			info.ReclaimPolicy = string(*c.ReclaimPolicy)
		}
		if c.VolumeBindingMode != nil {
			info.VolumeBindingMode = string(*c.VolumeBindingMode)
		}
		if c.AllowVolumeExpansion != nil {
			info.AllowVolumeExpansion = *c.AllowVolumeExpansion
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func describeBudget(pdb *policyv1.PodDisruptionBudget) DisruptionBudget {
	out := DisruptionBudget{
		Name:               pdb.Name,
		DisruptionsAllowed: pdb.Status.DisruptionsAllowed,
		CurrentHealthy:     pdb.Status.CurrentHealthy,
		DesiredHealthy:     pdb.Status.DesiredHealthy,
		ExpectedPods:       pdb.Status.ExpectedPods,
	}
	if v := pdb.Spec.MinAvailable; v != nil {
		out.MinAvailable = v.String()
	}
	if v := pdb.Spec.MaxUnavailable; v != nil {
		out.MaxUnavailable = v.String()
	}
	return out
}

func describeAutoscaler(hpa *autoscalingv2.HorizontalPodAutoscaler) Autoscaler {
	out := Autoscaler{
		Name:            hpa.Name,
		MinReplicas:     hpa.Spec.MinReplicas,
		MaxReplicas:     hpa.Spec.MaxReplicas,
		CurrentReplicas: hpa.Status.CurrentReplicas,
		DesiredReplicas: hpa.Status.DesiredReplicas,
	}
	for _, c := range hpa.Status.Conditions {
		if c.Type == autoscalingv2.ScalingLimited && c.Status == corev1.ConditionTrue {
			out.LimitedReason = c.Reason
		}
	}
	for _, m := range hpa.Spec.Metrics {
		if metric, ok := describeMetric(m); ok {
			out.Metrics = append(out.Metrics, metric)
		}
	}
	return out
}

// describeMetric reduces one metric spec to what it targets and at what value.
// A metric whose name does not fit the bound is dropped rather than truncated,
// the rule external strings follow everywhere in this package (ADR 0031 §5).
func describeMetric(m autoscalingv2.MetricSpec) (AutoscalerMetric, bool) {
	out := AutoscalerMetric{Type: string(m.Type)}
	var target autoscalingv2.MetricTarget
	switch m.Type {
	case autoscalingv2.ResourceMetricSourceType:
		if m.Resource == nil {
			return out, false
		}
		out.Name, target = string(m.Resource.Name), m.Resource.Target
	case autoscalingv2.ContainerResourceMetricSourceType:
		if m.ContainerResource == nil {
			return out, false
		}
		out.Name, target = string(m.ContainerResource.Name), m.ContainerResource.Target
	case autoscalingv2.PodsMetricSourceType:
		if m.Pods == nil {
			return out, false
		}
		out.Name, target = m.Pods.Metric.Name, m.Pods.Target
	case autoscalingv2.ObjectMetricSourceType:
		if m.Object == nil {
			return out, false
		}
		out.Name, target = m.Object.Metric.Name, m.Object.Target
	case autoscalingv2.ExternalMetricSourceType:
		if m.External == nil {
			return out, false
		}
		out.Name, target = m.External.Metric.Name, m.External.Target
	default:
		return out, false
	}
	if !fits(out.Name) {
		return out, false
	}
	out.TargetType = string(target.Type)
	switch {
	case target.AverageUtilization != nil:
		out.TargetValue = strconv.Itoa(int(*target.AverageUtilization))
	case target.AverageValue != nil:
		out.TargetValue = target.AverageValue.String()
	case target.Value != nil:
		out.TargetValue = target.Value.String()
	}
	return out, true
}

func describeClaim(pvc *corev1.PersistentVolumeClaim) VolumeClaim {
	out := VolumeClaim{
		Name:  pvc.Name,
		Phase: string(pvc.Status.Phase),
	}
	if pvc.Spec.StorageClassName != nil && fits(*pvc.Spec.StorageClassName) {
		out.StorageClass = *pvc.Spec.StorageClassName
	}
	for _, mode := range pvc.Spec.AccessModes {
		out.AccessModes = append(out.AccessModes, string(mode))
	}
	if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		out.RequestedBytes = q.Value()
	}
	return out
}

func describeLimitRange(lr *corev1.LimitRange) LimitRangeInfo {
	out := LimitRangeInfo{Name: lr.Name}
	for _, item := range lr.Spec.Limits {
		reduced := LimitRangeItem{
			Type:           string(item.Type),
			DefaultRequest: amountsOf(item.DefaultRequest),
			Default:        amountsOf(item.Default),
			Min:            amountsOf(item.Min),
			Max:            amountsOf(item.Max),
		}
		if reduced.DefaultRequest.empty() && reduced.Default.empty() &&
			reduced.Min.empty() && reduced.Max.empty() {
			continue
		}
		out.Items = append(out.Items, reduced)
	}
	return out
}

func amountsOf(list corev1.ResourceList) ResourceAmounts {
	var out ResourceAmounts
	if q, ok := list[corev1.ResourceCPU]; ok {
		out.CPUMilli = ptr.To(q.MilliValue())
	}
	if q, ok := list[corev1.ResourceMemory]; ok {
		out.MemoryBytes = ptr.To(q.Value())
	}
	if q, ok := list[corev1.ResourceStorage]; ok {
		out.StorageBytes = ptr.To(q.Value())
	}
	return out
}

func describeQuota(q *corev1.ResourceQuota) ResourceQuotaInfo {
	out := ResourceQuotaInfo{Name: q.Name}
	out.Hard = quantityMap(q.Spec.Hard)
	if len(q.Status.Hard) > 0 {
		// The effective ceiling is the one in status: a quota edited but not
		// yet reconciled would otherwise be reported as already in force.
		out.Hard = quantityMap(q.Status.Hard)
	}
	out.Used = quantityMap(q.Status.Used)
	for _, scope := range q.Spec.Scopes {
		out.Scopes = append(out.Scopes, string(scope))
	}
	sort.Strings(out.Scopes)
	return out
}

func quantityMap(list corev1.ResourceList) map[string]string {
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]string, len(list))
	for name, q := range list {
		if !fits(string(name)) {
			continue
		}
		out[string(name)] = q.String()
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// claimNames returns the PersistentVolumeClaim names a pod mounts.
//
// This is the one part of `spec.volumes` the agent reads, and it amends ADR
// 0031, which declined the field wholesale because the volume list references
// Secrets and ConfigMaps by name. Only entries whose source is a
// `persistentVolumeClaim` are read; a Secret, ConfigMap or projected volume in
// the same list is skipped without its name being touched. The reason ADR 0031
// gave still holds — it just does not reach this subset.
func claimNames(spec *corev1.PodSpec) []string {
	var out []string
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		if !fits(v.PersistentVolumeClaim.ClaimName) {
			continue
		}
		out = append(out, v.PersistentVolumeClaim.ClaimName)
	}
	return out
}
