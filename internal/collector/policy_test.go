package collector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

// storageClassSecretParam is a StorageClass parameter value standing in for the
// provider configuration operators put there. It is a constant so the byte-level
// test and the fixture cannot drift apart.
const storageClassSecretParam = "projects/acme/keys/prod-disk-key" //nolint:gosec // fixture provider config; the test asserts it never reaches the payload

func policyPod(name, deployment string, claims ...string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: name, UID: types.UID("uid-" + name),
			Labels:          map[string]string{"app": deployment},
			OwnerReferences: []metav1.OwnerReference{controllerRef("ReplicaSet", deployment+"-abc")},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "example.com/app:1"}},
			Volumes: []corev1.Volume{{
				// A Secret volume sits in the same list the claim names are
				// read from, and must not be touched (ADR 0032 §6).
				Name: "creds",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: "prod-db-credentials"}, //nolint:gosec // fixture Secret name; asserted absent from the payload
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable},
	}
	for _, claim := range claims {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
			Name: claim,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			},
		})
	}
	return p
}

func replicaSetOwnedBy(name, deployment string) runtime.Object {
	return replicaSet(name, deployment, "1", "example.com/app:1", 1, 1)
}

func budget(name, app, minAvailable string, allowed int32) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}},
			MinAvailable: ptr.To(intstr.Parse(minAvailable)),
		},
		Status: policyv1.PodDisruptionBudgetStatus{
			DisruptionsAllowed: allowed, CurrentHealthy: 2, DesiredHealthy: 2, ExpectedPods: 2,
		},
	}
}

func autoscaler(name, targetKind, targetName string, minReplicas, maxReplicas int32) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: targetKind, Name: targetName},
			MinReplicas:    ptr.To(minReplicas),
			MaxReplicas:    maxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: ptr.To(int32(80)),
					},
				},
			}},
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			CurrentReplicas: 8, DesiredReplicas: 8,
			Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{{
				Type: autoscalingv2.ScalingLimited, Status: corev1.ConditionTrue,
				Reason:  "TooManyReplicas",
				Message: "the desired replica count is more than the maximum replica count",
			}},
		},
	}
}

// collectPolicy runs a watcher over the given objects until ready reports that
// the state under test has arrived.
//
// The predicate belongs to the caller: the cluster-scoped catalogs are populated
// as soon as the informers sync, while anything derived from the pod index
// arrives only once a pod has been admitted. A shared "something is non-empty"
// condition would let a test read the catalogs and conclude the index was empty.
func collectPolicy(t *testing.T, filter *Filter, ready func([]WorkloadPolicy, ClusterPolicy) bool, objects ...runtime.Object) ([]WorkloadPolicy, ClusterPolicy) {
	t.Helper()
	clientset := fake.NewClientset(objects...)
	watcher := NewPodWatcher(clientset, func(PodInfo) {})
	watcher.SetFilter(filter)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	var policies []WorkloadPolicy
	var cluster ClusterPolicy
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		policies, _ = watcher.WorkloadPolicies()
		cluster, _ = watcher.ClusterPolicy()
		if ready(policies, cluster) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
	return policies, cluster
}

// hasRecords is the usual condition: the pod index has produced a policy record.
func hasRecords(policies []WorkloadPolicy, _ ClusterPolicy) bool { return len(policies) > 0 }

// A budget names pods by label selector, not by workload, so the join runs
// through the admitted pod index.
func TestBudgetAttachesToTheWorkloadOfTheSelectedPods(t *testing.T) {
	got, _ := collectPolicy(t, NewFilter(nil, nil), hasRecords,
		policyPod("web-1", "web"),
		replicaSetOwnedBy("web-abc", "web"),
		budget("web-pdb", "web", "100%", 0),
	)
	if len(got) != 1 {
		t.Fatalf("records = %d, want 1: %+v", len(got), got)
	}
	if got[0].Workload.Name != "web" || got[0].Workload.Kind != "Deployment" {
		t.Errorf("workload = %+v", got[0].Workload)
	}
	if len(got[0].Budgets) != 1 {
		t.Fatalf("budgets = %+v", got[0].Budgets)
	}
	b := got[0].Budgets[0]
	// Zero disruptions allowed is the fact that matters: the node cannot be
	// drained today whatever the declaration permits in principle.
	if b.MinAvailable != "100%" || b.DisruptionsAllowed != 0 {
		t.Errorf("budget = %+v", b)
	}
}

// A budget covering only excluded pods must attach to nothing. Otherwise the
// payload would name a workload the customer asked not to be measured
// (CLAUDE.md invariant 6).
func TestBudgetOverExcludedPodsNamesNothing(t *testing.T) {
	filter := NewFilter(nil, []string{"shop"})
	got, cluster := collectPolicy(t, filter,
		// The pod is observed and rejected; waiting for a record would wait
		// for something that must never arrive.
		func([]WorkloadPolicy, ClusterPolicy) bool { return filter.Snapshot().PodsObserved > 0 },
		policyPod("web-1", "web"),
		replicaSetOwnedBy("web-abc", "web"),
		budget("web-pdb", "web", "100%", 0),
		&corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "defaults"}},
	)
	if len(got) != 0 {
		t.Errorf("records = %+v, want none for an excluded namespace", got)
	}
	if len(cluster.Namespaces) != 0 {
		t.Errorf("cluster namespaces = %+v, want none", cluster.Namespaces)
	}
}

// An autoscaler names its workload directly, so the join is exact — and a
// scaleTargetRef pointing at a different kind of object with the same name must
// not match.
func TestAutoscalerJoinsItsTargetExactly(t *testing.T) {
	got, _ := collectPolicy(t, NewFilter(nil, nil), hasRecords,
		policyPod("web-1", "web"),
		replicaSetOwnedBy("web-abc", "web"),
		autoscaler("web-hpa", "Deployment", "web", 3, 8),
		autoscaler("other-hpa", "StatefulSet", "web", 1, 2),
	)
	if len(got) != 1 {
		t.Fatalf("records = %d: %+v", len(got), got)
	}
	if len(got[0].Autoscalers) != 1 || got[0].Autoscalers[0].Name != "web-hpa" {
		t.Fatalf("autoscalers = %+v, want only the Deployment-targeting one", got[0].Autoscalers)
	}
	hpa := got[0].Autoscalers[0]
	if hpa.MaxReplicas != 8 || hpa.CurrentReplicas != 8 || hpa.LimitedReason != "TooManyReplicas" {
		t.Errorf("autoscaler = %+v", hpa)
	}
	// The utilization target must travel with its type: 80 means nothing
	// without "percentage of the request" attached to it.
	if len(hpa.Metrics) != 1 {
		t.Fatalf("metrics = %+v", hpa.Metrics)
	}
	m := hpa.Metrics[0]
	if m.Name != "cpu" || m.TargetType != "Utilization" || m.TargetValue != "80" {
		t.Errorf("metric = %+v", m)
	}
}

func TestClaimsResolveThroughThePodsVolumes(t *testing.T) {
	got, _ := collectPolicy(t, NewFilter(nil, nil), hasRecords,
		policyPod("db-1", "db", "data"),
		replicaSetOwnedBy("db-abc", "db"),
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data"},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: ptr.To("gp3-zonal"),
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Gi")},
				},
			},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		},
	)
	if len(got) != 1 || len(got[0].Claims) != 1 {
		t.Fatalf("records = %+v", got)
	}
	c := got[0].Claims[0]
	if c.Name != "data" || c.StorageClass != "gp3-zonal" || c.Phase != "Bound" {
		t.Errorf("claim = %+v", c)
	}
	if c.RequestedBytes != 100*1024*1024*1024 {
		t.Errorf("requested bytes = %d", c.RequestedBytes)
	}
}

// A workload nothing constrains produces no record. The payload is a snapshot,
// so an absent workload reads as "nothing bounds it", which is true — unlike a
// journal, where silence would mean a quiet window.
func TestUnconstrainedWorkloadHasNoPolicyRecord(t *testing.T) {
	got, _ := collectPolicy(t, NewFilter(nil, nil),
		// Nothing constrains this workload, so no record will ever appear;
		// wait on the catalog instead of on a condition that cannot arrive.
		func(_ []WorkloadPolicy, c ClusterPolicy) bool { return len(c.PriorityClasses) > 0 },
		policyPod("web-1", "web"),
		replicaSetOwnedBy("web-abc", "web"),
		&schedulingv1.PriorityClass{
			ObjectMeta: metav1.ObjectMeta{Name: "high"}, Value: 1000,
		},
	)
	if len(got) != 0 {
		t.Errorf("records = %+v, want none", got)
	}
}

func TestClusterPolicyCarriesNamespaceLimitsAndTheCatalogs(t *testing.T) {
	_, cluster := collectPolicy(t, NewFilter(nil, nil),
		func(_ []WorkloadPolicy, c ClusterPolicy) bool { return len(c.Namespaces) > 0 },
		policyPod("web-1", "web"),
		replicaSetOwnedBy("web-abc", "web"),
		&corev1.LimitRange{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "defaults"},
			Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
				Type:           corev1.LimitTypeContainer,
				DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				Default:        corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			}}},
		},
		&corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "team"},
			Status: corev1.ResourceQuotaStatus{
				Hard: corev1.ResourceList{"requests.cpu": resource.MustParse("40")},
				Used: corev1.ResourceList{"requests.cpu": resource.MustParse("31")},
			},
		},
		&schedulingv1.PriorityClass{
			ObjectMeta: metav1.ObjectMeta{Name: "high"},
			Value:      1000, GlobalDefault: false,
			PreemptionPolicy: ptr.To(corev1.PreemptLowerPriority),
		},
		&storagev1.StorageClass{
			ObjectMeta:           metav1.ObjectMeta{Name: "gp3-zonal"},
			Provisioner:          "ebs.csi.aws.com",
			VolumeBindingMode:    ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
			AllowVolumeExpansion: ptr.To(true),
			Parameters:           map[string]string{"kmsKeyId": storageClassSecretParam},
		},
	)

	if len(cluster.Namespaces) != 1 || cluster.Namespaces[0].Namespace != "shop" {
		t.Fatalf("namespaces = %+v", cluster.Namespaces)
	}
	ns := cluster.Namespaces[0]
	if len(ns.LimitRanges) != 1 || len(ns.LimitRanges[0].Items) != 1 {
		t.Fatalf("limit ranges = %+v", ns.LimitRanges)
	}
	// A request of 100m in workload metadata may be a team's choice or this
	// namespace's default. Without the LimitRange the two are indistinguishable.
	item := ns.LimitRanges[0].Items[0]
	if item.DefaultRequest.CPUMilli == nil || *item.DefaultRequest.CPUMilli != 100 {
		t.Errorf("default request = %+v", item.DefaultRequest)
	}
	if item.Default.MemoryBytes == nil || *item.Default.MemoryBytes != 512*1024*1024 {
		t.Errorf("default limit = %+v", item.Default)
	}
	if len(ns.Quotas) != 1 || ns.Quotas[0].Hard["requests.cpu"] != "40" ||
		ns.Quotas[0].Used["requests.cpu"] != "31" {
		t.Errorf("quotas = %+v", ns.Quotas)
	}

	if len(cluster.PriorityClasses) != 1 || cluster.PriorityClasses[0].Value != 1000 {
		t.Errorf("priority classes = %+v", cluster.PriorityClasses)
	}
	if len(cluster.StorageClasses) != 1 {
		t.Fatalf("storage classes = %+v", cluster.StorageClasses)
	}
	sc := cluster.StorageClasses[0]
	// WaitForFirstConsumer is what decides whether a claim becomes a placement
	// constraint before or after scheduling.
	if sc.VolumeBindingMode != "WaitForFirstConsumer" || !sc.AllowVolumeExpansion {
		t.Errorf("storage class = %+v", sc)
	}
}

// Checked on the encoded bytes rather than the shape of the structs: a
// StorageClass's parameters carry provider configuration, a Secret volume sits
// in the same list the claim names come from, and an HPA condition's message is
// free text (ADR 0020 §6). None may reach the payload.
func TestPolicyPayloadCarriesNoOperatorSecrets(t *testing.T) {
	policies, cluster := collectPolicy(t, NewFilter(nil, nil), hasRecords,
		policyPod("db-1", "db", "data"),
		replicaSetOwnedBy("db-abc", "db"),
		autoscaler("db-hpa", "Deployment", "db", 1, 4),
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To("gp3-zonal")},
		},
		&storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "gp3-zonal"},
			Provisioner: "ebs.csi.aws.com",
			Parameters:  map[string]string{"kmsKeyId": storageClassSecretParam},
		},
	)

	encoded, err := json.Marshal(struct {
		Workloads []WorkloadPolicy `json:"workloads"`
		Cluster   ClusterPolicy    `json:"cluster"`
	}{policies, cluster})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		storageClassSecretParam, "kmsKeyId",
		"prod-db-credentials", "creds",
		"the desired replica count is more than the maximum replica count",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("policy payload contains %q:\n%s", forbidden, encoded)
		}
	}
}

// A denied permission must degrade one payload, not stop the agent. Before
// ADR 0033 the policy caches were in the blocking sync list, so a customer who
// removed one line from the ClusterRole lost the collection of everything —
// usage included.
func TestDeniedPolicySourceDegradesInsteadOfStoppingTheAgent(t *testing.T) {
	clientset := fake.NewClientset(
		policyPod("web-1", "web"),
		replicaSetOwnedBy("web-abc", "web"),
		budget("web-pdb", "web", "100%", 0),
	)
	forbid := func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "policy", Resource: "poddisruptionbudgets"},
			"", errors.New("no permission"))
	}
	clientset.PrependReactor("list", "poddisruptionbudgets", forbid)
	clientset.PrependWatchReactor("poddisruptionbudgets", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "policy", Resource: "poddisruptionbudgets"},
			"", errors.New("no permission"))
	})

	watcher := NewPodWatcher(clientset, func(PodInfo) {})
	watcher.SetFilter(NewFilter(nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	// The agent must reach the point of collecting pods at all — that is the
	// property the old code destroyed.
	var pods []PodInfo
	var unavailable []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pods = watcher.Pods()
		_, unavailable = watcher.WorkloadPolicies()
		if len(pods) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher stopped on a denied policy source: %v", err)
	}
	if len(pods) == 0 {
		t.Fatal("no pods collected: the denied source stopped unrelated collection")
	}
	if len(unavailable) != 1 || unavailable[0] != "pod_disruption_budgets" {
		t.Errorf("unavailable sources = %v, want the denied one named", unavailable)
	}
}

// The distinction the whole mechanism rests on: a cluster that simply has no
// budgets is not the same state as one that refused to show them. HasSynced
// separates them because an empty list still syncs.
func TestEmptyClusterReportsNoUnavailableSources(t *testing.T) {
	_, unavailable := func() ([]WorkloadPolicy, []string) {
		clientset := fake.NewClientset(
			policyPod("web-1", "web"),
			replicaSetOwnedBy("web-abc", "web"),
		)
		watcher := NewPodWatcher(clientset, func(PodInfo) {})
		watcher.SetFilter(NewFilter(nil, nil))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- watcher.Run(ctx) }()

		var out []WorkloadPolicy
		var gaps []string
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			out, gaps = watcher.WorkloadPolicies()
			if len(watcher.Pods()) > 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("watcher returned error: %v", err)
		}
		return out, gaps
	}()
	if len(unavailable) != 0 {
		t.Errorf("unavailable sources = %v, want none: every cache synced over an empty cluster", unavailable)
	}
}

// Each payload declares its own sources. The storage-class catalog belongs to
// cluster policy alone: a claim's storage_class is a name read from the
// PersistentVolumeClaim, and the catalog only says what that name means.
func TestEachPayloadDeclaresOnlyItsOwnSources(t *testing.T) {
	w := &PodWatcher{policySources: []policySource{
		{name: "pod_disruption_budgets", synced: func() bool { return false }},
		{name: "horizontal_pod_autoscalers", synced: func() bool { return true }},
		{name: "persistent_volume_claims", synced: func() bool { return true }},
		{name: "limit_ranges", synced: func() bool { return true }},
		{name: "resource_quotas", synced: func() bool { return true }},
		{name: "priority_classes", synced: func() bool { return true }},
		{name: "storage_classes", synced: func() bool { return false }},
	}}
	workload := w.unavailablePolicySources(workloadPolicySources...)
	if len(workload) != 1 || workload[0] != "pod_disruption_budgets" {
		t.Errorf("workload payload sources = %v", workload)
	}
	cluster := w.unavailablePolicySources(clusterPolicySources...)
	if len(cluster) != 1 || cluster[0] != "storage_classes" {
		t.Errorf("cluster payload sources = %v", cluster)
	}
}
