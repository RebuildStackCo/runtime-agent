package collector

import (
	"sort"
	"strconv"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
)

// amountsEmpty reports whether a reduced amount set carries nothing. It is the
// reduction's own question, so it lives here rather than on the type.
func amountsEmpty(r model.ResourceAmounts) bool {
	return r.CPUMilli == nil && r.MemoryBytes == nil && r.StorageBytes == nil
}

// WorkloadPolicies returns one record per workload that has admitted pods.
//
// The scope is inherited from the admitted pod index rather than decided again,
// the same construction ADR 0030 used for revisions: a workload excluded by any
// filter has no admitted pods and therefore cannot appear here, and there is no
// second admission path to keep in step.
func (w *PodWatcher) WorkloadPolicies() ([]model.WorkloadPolicy, []string) {
	unavailable := w.unavailablePolicySources(workloadPolicySources...)
	workloads, claimsByWorkload := w.collectedWorkloads()
	if len(workloads) == 0 {
		return nil, unavailable
	}

	byKey := make(map[workloadKey]*model.WorkloadPolicy, len(workloads))
	for key, ref := range workloads {
		byKey[key] = &model.WorkloadPolicy{Namespace: key.namespace, Workload: ref}
	}

	w.attachBudgets(byKey)
	w.attachAutoscalers(byKey)
	w.attachClaims(byKey, claimsByWorkload)
	w.attachServices(byKey)

	out := make([]model.WorkloadPolicy, 0, len(byKey))
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
		"services", "endpoint_slices", "pod_disruption_budgets",
		"horizontal_pod_autoscalers", "persistent_volume_claims",
	}
	clusterPolicySources = []string{
		"limit_ranges", "resource_quotas", "priority_classes", "storage_classes",
	}
)

// collectedWorkloads reads the admitted pod index once, returning every workload
// with admitted pods and the claim names those pods mount.
func (w *PodWatcher) collectedWorkloads() (map[workloadKey]model.WorkloadRef, map[workloadKey][]string) {
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()

	workloads := make(map[workloadKey]model.WorkloadRef)
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
func (w *PodWatcher) attachBudgets(byKey map[workloadKey]*model.WorkloadPolicy) {
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
func (w *PodWatcher) attachServices(byKey map[workloadKey]*model.WorkloadPolicy) {
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
		reduced.Endpoints = w.endpointZones(svc.Namespace, svc.Name)
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
func describeService(svc *corev1.Service) model.ServiceExposure {
	return model.ServiceExposure{
		Name:                  svc.Name,
		Type:                  string(svc.Spec.Type),
		Headless:              svc.Spec.ClusterIP == corev1.ClusterIPNone,
		InternalTrafficPolicy: internalTrafficPolicy(svc),
		ExternalTrafficPolicy: string(svc.Spec.ExternalTrafficPolicy),
		TrafficDistribution:   ptr.Deref(svc.Spec.TrafficDistribution, ""),
		TopologyMode:          topologyMode(svc),
	}
}

// The two annotations that asked for topology-aware routing before
// `spec.trafficDistribution` existed. Both are still set on live clusters, and
// the older one is read second so a Service carrying both reports the current
// spelling.
var topologyModeAnnotations = []string{
	"service.kubernetes.io/topology-mode",
	"service.kubernetes.io/topology-aware-hints",
}

// maxTopologyModeValue bounds what is kept. The vocabulary is "Auto" and
// "Disabled"; anything past this is not a mode, and a prefix of an unexpected
// string would ship looking like one (ADR 0019's rule, applied to an
// annotation).
const maxTopologyModeValue = 32

func topologyMode(svc *corev1.Service) string {
	for _, name := range topologyModeAnnotations {
		if v, ok := svc.Annotations[name]; ok && v != "" && len(v) <= maxTopologyModeValue {
			return v
		}
	}
	return ""
}

func internalTrafficPolicy(svc *corev1.Service) string {
	if svc.Spec.InternalTrafficPolicy == nil {
		return ""
	}
	return string(*svc.Spec.InternalTrafficPolicy)
}

// endpointZones counts one Service's ready endpoints by zone, one entry per
// address family, sorted by family so the payload bytes are deterministic.
//
// Slices are found by the label the EndpointSlice controller sets, which is the
// only link the agent needs: the entries it reads carry a zone and nothing that
// identifies the pod behind it — the cache transform removed the rest before
// this object was stored (ADR 0051 §1).
func (w *PodWatcher) endpointZones(namespace, service string) []model.EndpointZones {
	if w.epsLister == nil {
		return nil
	}
	selector := labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: service})
	slices, err := w.epsLister.EndpointSlices(namespace).List(selector)
	if err != nil || len(slices) == 0 {
		return nil
	}

	byFamily := make(map[string]*model.EndpointZones)
	for _, slice := range slices {
		family := string(slice.AddressType)
		z, ok := byFamily[family]
		if !ok {
			z = &model.EndpointZones{AddressType: family, Zones: make(map[string]int)}
			byFamily[family] = z
		}
		for i := range slice.Endpoints {
			e := &slice.Endpoints[i]
			// An unset ready condition means ready: the API's instruction to a
			// consumer that does not understand the condition, and reading it
			// the other way would under-count every zone.
			if !ptr.Deref(e.Conditions.Ready, true) {
				continue
			}
			z.Ready++
			if zone := ptr.Deref(e.Zone, ""); zone != "" {
				z.Zones[zone]++
			} else {
				z.Unzoned++
			}
			if e.Hints != nil && len(e.Hints.ForZones) > 0 {
				z.Hinted++
			}
		}
	}

	out := make([]model.EndpointZones, 0, len(byFamily))
	for _, z := range byFamily {
		if len(z.Zones) == 0 {
			z.Zones = nil
		}
		out = append(out, *z)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AddressType < out[j].AddressType })
	return out
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
func (w *PodWatcher) attachAutoscalers(byKey map[workloadKey]*model.WorkloadPolicy) {
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

func (w *PodWatcher) attachClaims(byKey map[workloadKey]*model.WorkloadPolicy, claimsByWorkload map[workloadKey][]string) {
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
func (w *PodWatcher) ClusterPolicy() (model.ClusterPolicy, []string) {
	unavailable := w.unavailablePolicySources(clusterPolicySources...)
	var out model.ClusterPolicy
	for _, ns := range w.collectedNamespaces() {
		policy := model.NamespacePolicy{Namespace: ns}
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

func (w *PodWatcher) limitRangesIn(namespace string) []model.LimitRangeInfo {
	if w.limitRangeLister == nil {
		return nil
	}
	ranges, err := w.limitRangeLister.LimitRanges(namespace).List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]model.LimitRangeInfo, 0, len(ranges))
	for _, lr := range ranges {
		out = append(out, describeLimitRange(lr))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (w *PodWatcher) quotasIn(namespace string) []model.ResourceQuotaInfo {
	if w.quotaLister == nil {
		return nil
	}
	quotas, err := w.quotaLister.ResourceQuotas(namespace).List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]model.ResourceQuotaInfo, 0, len(quotas))
	for _, q := range quotas {
		out = append(out, describeQuota(q))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (w *PodWatcher) priorityClasses() []model.PriorityClassInfo {
	if w.priorityLister == nil {
		return nil
	}
	classes, err := w.priorityLister.List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]model.PriorityClassInfo, 0, len(classes))
	for _, c := range classes {
		if !fits(c.Name) {
			continue
		}
		info := model.PriorityClassInfo{
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

func (w *PodWatcher) storageClasses() []model.StorageClassInfo {
	if w.storageClassLister == nil {
		return nil
	}
	classes, err := w.storageClassLister.List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]model.StorageClassInfo, 0, len(classes))
	for _, c := range classes {
		if !fits(c.Name) || !fits(c.Provisioner) {
			continue
		}
		info := model.StorageClassInfo{
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

func describeBudget(pdb *policyv1.PodDisruptionBudget) model.DisruptionBudget {
	out := model.DisruptionBudget{
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

func describeAutoscaler(hpa *autoscalingv2.HorizontalPodAutoscaler) model.Autoscaler {
	out := model.Autoscaler{
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
func describeMetric(m autoscalingv2.MetricSpec) (model.AutoscalerMetric, bool) {
	out := model.AutoscalerMetric{Type: string(m.Type)}
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

func describeClaim(pvc *corev1.PersistentVolumeClaim) model.VolumeClaim {
	out := model.VolumeClaim{
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

func describeLimitRange(lr *corev1.LimitRange) model.LimitRangeInfo {
	out := model.LimitRangeInfo{Name: lr.Name}
	for _, item := range lr.Spec.Limits {
		reduced := model.LimitRangeItem{
			Type:           string(item.Type),
			DefaultRequest: amountsOf(item.DefaultRequest),
			Default:        amountsOf(item.Default),
			Min:            amountsOf(item.Min),
			Max:            amountsOf(item.Max),
		}
		if amountsEmpty(reduced.DefaultRequest) && amountsEmpty(reduced.Default) &&
			amountsEmpty(reduced.Min) && amountsEmpty(reduced.Max) {
			continue
		}
		out.Items = append(out.Items, reduced)
	}
	return out
}

func amountsOf(list corev1.ResourceList) model.ResourceAmounts {
	var out model.ResourceAmounts
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

func describeQuota(q *corev1.ResourceQuota) model.ResourceQuotaInfo {
	out := model.ResourceQuotaInfo{Name: q.Name}
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
