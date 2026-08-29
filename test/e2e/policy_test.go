//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
)

const (
	revisionsSpoolPath      = spoolDir + "/workload-revisions.json"
	workloadPolicySpoolPath = spoolDir + "/workload-policy.json"
	clusterPolicySpoolPath  = spoolDir + "/cluster-policy.json"
	jobRunsSpoolGlob        = spoolDir + "/job-runs-*.json"
	workloadMetadataPath    = spoolDir + "/workload-metadata.json"
)

// probeSecret is written into a probe's exec command in the fixture and asserted
// absent from every payload: a probe handler is the second place a command is
// written (ADR 0048 §1).
// #nosec G101 -- a fixture marker, and finding it in a payload is the failure
const probeSecret = "--token=e2e-probe-must-not-ship"

// TestPolicyAndJournalsEndToEnd exercises the four payload kinds that had golden
// bytes but had never met a real API server: `job_runs` (ADR 0029),
// `deployment_revisions` (ADR 0030), `workload_policy` and `cluster_policy`
// (ADR 0032).
//
// Workload policy earns the test: since ADR 0033 a missing grant no longer stops
// the agent, so it would be silent everywhere else and surfaces here as an
// `unavailable_sources` line this test requires to be absent.
func TestPolicyAndJournalsEndToEnd(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	if agentImage == "" {
		t.Skip("E2E_AGENT_IMAGE not set; run `make policy-e2e`")
	}
	config := clusterConfig(t)
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-policy-e2e-%d", os.Getpid())
	fixtureNS := ns + "-fixtures"
	for _, name := range []string{ns, fixtureNS} {
		if _, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating namespace %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cleanupCancel()
		for _, name := range []string{ns, fixtureNS} {
			_ = clientset.CoreV1().Namespaces().Delete(cleanupCtx, name, metav1.DeleteOptions{})
		}
	})

	createPolicyFixtures(ctx, t, clientset, fixtureNS)

	deployController(ctx, t, clientset, ns, agentImage)
	controllerPod := waitDeploymentPod(ctx, t, clientset, ns, "controller")
	t.Logf("controller pod: %s", controllerPod)

	checkRevisionsReported(ctx, t, config, clientset, ns, controllerPod, fixtureNS)
	checkWorkloadPolicyReported(ctx, t, config, clientset, ns, controllerPod, fixtureNS)
	checkClusterPolicyReported(ctx, t, config, clientset, ns, controllerPod, fixtureNS)
	checkJobRunReported(ctx, t, config, clientset, ns, controllerPod, fixtureNS)
	checkWorkloadShapeReported(ctx, t, config, clientset, ns, controllerPod, fixtureNS)
}

// createPolicyFixtures builds one workload wrapped in every policy object the
// two payloads read, plus a Job that finishes on its own.
func createPolicyFixtures(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns string) {
	t.Helper()

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(2)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			// An update that replaces everything at once. No eviction is
			// involved, so the budget above does not bound it (ADR 0048 §2).
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: ptr.To(intstr.FromString("100%")),
					MaxSurge:       ptr.To(intstr.FromInt32(0)),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec: corev1.PodSpec{
					// Placement the agent must report alongside the policy:
					// required anti-affinity on hostname is the constraint that
					// makes the workload unpackable (ADR 0031).
					Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
							Weight: 100,
							PodAffinityTerm: corev1.PodAffinityTerm{
								TopologyKey:   "kubernetes.io/hostname",
								LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
							},
						}},
					}},
					Containers: []corev1.Container{{
						Name:    "app",
						Image:   "registry.k8s.io/pause:3.9",
						Command: []string{"/pause"},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
						},
						// The probe never succeeds against pause, which does not
						// matter: the schedule is the fact under test, and the
						// command is what must not come back (ADR 0048 §1).
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
								Command: []string{"/bin/true", probeSecret},
							}},
							PeriodSeconds: 11, FailureThreshold: 4,
						},
					}},
				},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	if _, err := cs.CoreV1().Services(ns).Create(ctx, service, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating service: %v", err)
	}

	// A ReplicaSet under a controller kind that has no CRD in this cluster. The
	// garbage collector cannot resolve the owner and leaves the object alone, so
	// this is the shape an Argo Rollout produces without installing Argo.
	widgetRS := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "payments-abc123",
			Annotations: map[string]string{"made.up.io/revision": "7"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "made.up.io/v1", Kind: "Widget", Name: "payments",
				UID: "11111111-2222-3333-4444-555555555555", Controller: ptr.To(true),
			}},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "payments"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "payments"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "app", Image: "registry.k8s.io/pause:3.9", Command: []string{"/pause"},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m")},
					},
				}}},
			},
		},
	}
	if _, err := cs.AppsV1().ReplicaSets(ns).Create(ctx, widgetRS, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the widget-owned replicaset: %v", err)
	}

	budget := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-pdb"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr.To(intstr.FromString("100%")),
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		},
	}
	if _, err := cs.PolicyV1().PodDisruptionBudgets(ns).Create(ctx, budget, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating pod disruption budget: %v", err)
	}

	// No metrics server runs in kind, so the autoscaler will not scale. Its
	// spec is the fact under test — the agent reads the declaration, not the
	// scaling.
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-hpa"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "web",
			},
			MinReplicas: ptr.To(int32(2)),
			MaxReplicas: 5,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: ptr.To(int32(75)),
					},
				},
			}},
		},
	}
	if _, err := cs.AutoscalingV2().HorizontalPodAutoscalers(ns).Create(ctx, hpa, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating autoscaler: %v", err)
	}

	limits := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "defaults"},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m")},
			Default:        corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		}}},
	}
	if _, err := cs.CoreV1().LimitRanges(ns).Create(ctx, limits, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating limit range: %v", err)
	}

	// Deliberately generous: the quota must be readable, not binding. A quota
	// that blocked the fixtures would make this a test of the quota.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "team"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourcePods: resource.MustParse("50"),
		}},
	}
	if _, err := cs.CoreV1().ResourceQuotas(ns).Create(ctx, quota, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating resource quota: %v", err)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "nightly"},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(0)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "run",
						Image: "registry.k8s.io/busybox:1.27.2",
						Args:  []string{"/bin/true"},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
						},
					}},
				},
			},
		},
	}
	if _, err := cs.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating job: %v", err)
	}
}

// checkRevisionsReported waits for the Deployment's ReplicaSet to appear in the
// revisions snapshot with its revision number and image (ADR 0030).
func checkRevisionsReported(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod, fixtureNS string) {
	t.Helper()
	var payload struct {
		Kind    string `json:"kind"`
		Records []struct {
			Namespace  string                      `json:"namespace"`
			Workload   struct{ Kind, Name string } `json:"workload"`
			Revision   *int64                      `json:"revision"`
			Containers []struct {
				Name, Image string
			} `json:"containers"`
		} `json:"records"`
	}
	waitForSpoolJSON(ctx, t, config, cs, ns, pod, revisionsSpoolPath, &payload, func() bool {
		for _, r := range payload.Records {
			if r.Namespace == fixtureNS && r.Workload.Name == "web" {
				return r.Revision != nil && len(r.Containers) > 0
			}
		}
		return false
	}, "workload revisions for the fixture workload")

	if payload.Kind != "workload_revisions" {
		t.Errorf("kind = %q", payload.Kind)
	}
	for _, r := range payload.Records {
		if r.Namespace != fixtureNS || r.Workload.Name != "web" {
			continue
		}
		if r.Workload.Kind != "Deployment" {
			t.Errorf("workload kind = %q", r.Workload.Kind)
		}
		if *r.Revision != 1 {
			t.Errorf("revision = %d, want the first one", *r.Revision)
		}
		if !strings.Contains(r.Containers[0].Image, "pause") {
			t.Errorf("container image = %q", r.Containers[0].Image)
		}
		t.Logf("revision %d of %s/%s runs %s", *r.Revision, r.Namespace, r.Workload.Name, r.Containers[0].Image)
	}

	// The widening, against a controller kind that does not exist in this
	// cluster: a dangling owner reference is exactly what the agent sees for an
	// Argo Rollout, without installing one (ADR 0049 §1).
	found := false
	for _, r := range payload.Records {
		if r.Namespace != fixtureNS || r.Workload.Kind != "Widget" {
			continue
		}
		found = true
		if r.Revision != nil {
			t.Errorf("revision = %d for a %s; the key belongs to that controller, not to us",
				*r.Revision, r.Workload.Kind)
		}
		if len(r.Containers) == 0 || r.Workload.Name != "payments" {
			t.Errorf("record = %+v, want the Widget's set with its containers", r)
		}
		t.Logf("revision of %s/%s (kind %s) runs %s, unnumbered",
			r.Namespace, r.Workload.Name, r.Workload.Kind, r.Containers[0].Image)
	}
	if !found {
		t.Error("no revision for the Widget-owned ReplicaSet; the widening did not reach the payload")
	}
}

// checkWorkloadPolicyReported asserts the budget and the autoscaler reach the
// payload — and that no source is declared unavailable, which is what proves
// the six ClusterRole grants of ADR 0032 actually work against a real API
// server. Since ADR 0033 a missing grant degrades silently everywhere else.
func checkWorkloadPolicyReported(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod, fixtureNS string) {
	t.Helper()
	var payload struct {
		Kind               string   `json:"kind"`
		UnavailableSources []string `json:"unavailable_sources"`
		Records            []struct {
			Namespace string                      `json:"namespace"`
			Workload  struct{ Kind, Name string } `json:"workload"`
			Budgets   []struct {
				Name               string `json:"name"`
				MinAvailable       string `json:"min_available"`
				DisruptionsAllowed int32  `json:"disruptions_allowed"`
				ExpectedPods       int32  `json:"expected_pods"`
			} `json:"budgets"`
			Autoscalers []struct {
				Name        string `json:"name"`
				MinReplicas *int32 `json:"min_replicas"`
				MaxReplicas int32  `json:"max_replicas"`
				Metrics     []struct {
					Name        string `json:"name"`
					TargetType  string `json:"target_type"`
					TargetValue string `json:"target_value"`
				} `json:"metrics"`
			} `json:"autoscalers"`
			Services []struct {
				Name     string `json:"name"`
				Type     string `json:"type"`
				Headless bool   `json:"headless"`
			} `json:"services"`
		} `json:"records"`
	}
	find := func() (int, bool) {
		for i, r := range payload.Records {
			if r.Namespace == fixtureNS && r.Workload.Name == "web" &&
				len(r.Budgets) > 0 && len(r.Autoscalers) > 0 && len(r.Services) > 0 {
				return i, true
			}
		}
		return 0, false
	}
	waitForSpoolJSON(ctx, t, config, cs, ns, pod, workloadPolicySpoolPath, &payload, func() bool {
		_, ok := find()
		return ok
	}, "workload policy with a budget, an autoscaler and a service")

	if payload.Kind != "workload_policy" {
		t.Errorf("kind = %q", payload.Kind)
	}
	// The load-bearing assertion of this file.
	if len(payload.UnavailableSources) != 0 {
		t.Errorf("unavailable sources = %v; the deployed ClusterRole does not grant what ADR 0032 says it does",
			payload.UnavailableSources)
	}

	i, _ := find()
	record := payload.Records[i]
	budget := record.Budgets[0]
	if budget.Name != "web-pdb" || budget.MinAvailable != "100%" {
		t.Errorf("budget = %+v", budget)
	}
	// Something routes to this workload, which is what makes its replica count
	// an availability decision rather than a batch size (ADR 0048 §4).
	svc := record.Services[0]
	if svc.Name != "web" || svc.Type != "ClusterIP" || svc.Headless {
		t.Errorf("service = %+v, want the ClusterIP named web", svc)
	}
	// A budget demanding every replica leaves no room to evict one: the node
	// cannot be drained, whatever spare capacity exists elsewhere.
	if budget.DisruptionsAllowed != 0 {
		t.Errorf("disruptions allowed = %d, want 0 under minAvailable 100%%", budget.DisruptionsAllowed)
	}
	if budget.ExpectedPods == 0 {
		t.Errorf("expected pods = 0; the budget matched nothing")
	}

	hpa := record.Autoscalers[0]
	if hpa.Name != "web-hpa" || hpa.MaxReplicas != 5 {
		t.Errorf("autoscaler = %+v", hpa)
	}
	if len(hpa.Metrics) != 1 {
		t.Fatalf("autoscaler metrics = %+v", hpa.Metrics)
	}
	// The value is meaningless without its type: 75 is a percentage of the
	// request, which is why lowering the request changes scaling behavior.
	if m := hpa.Metrics[0]; m.Name != "cpu" || m.TargetType != "Utilization" || m.TargetValue != "75" {
		t.Errorf("autoscaler metric = %+v", m)
	}
	t.Logf("policy for %s/web: budget %s allows %d disruptions, autoscaler %s caps at %d, service %s (%s) routes to it",
		fixtureNS, budget.Name, budget.DisruptionsAllowed, hpa.Name, hpa.MaxReplicas, svc.Name, svc.Type)
}

func checkClusterPolicyReported(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod, fixtureNS string) {
	t.Helper()
	var payload struct {
		Kind               string   `json:"kind"`
		UnavailableSources []string `json:"unavailable_sources"`
		Policy             struct {
			Namespaces []struct {
				Namespace   string `json:"namespace"`
				LimitRanges []struct {
					Name  string `json:"name"`
					Items []struct {
						Type           string `json:"type"`
						DefaultRequest struct {
							CPUMilli *int64 `json:"cpu_milli"`
						} `json:"default_request"`
					} `json:"items"`
				} `json:"limit_ranges"`
				Quotas []struct {
					Name string            `json:"name"`
					Hard map[string]string `json:"hard"`
				} `json:"quotas"`
			} `json:"namespaces"`
			StorageClasses []struct {
				Name        string `json:"name"`
				Provisioner string `json:"provisioner"`
			} `json:"storage_classes"`
		} `json:"policy"`
	}
	findNS := func() (int, bool) {
		for i, n := range payload.Policy.Namespaces {
			if n.Namespace == fixtureNS && len(n.LimitRanges) > 0 && len(n.Quotas) > 0 {
				return i, true
			}
		}
		return 0, false
	}
	// The wait deliberately does not require the catalog. A ClusterRole missing
	// the storage-class grant would then be reported as a six-minute timeout
	// instead of as the one-line cause it is — so the payload is awaited on the
	// namespace alone, and the grants are asserted below where the failure can
	// name itself.
	waitForSpoolJSON(ctx, t, config, cs, ns, pod, clusterPolicySpoolPath, &payload, func() bool {
		_, ok := findNS()
		return ok
	}, "cluster policy with the fixture namespace")

	if payload.Kind != "cluster_policy" {
		t.Errorf("kind = %q", payload.Kind)
	}
	if len(payload.UnavailableSources) != 0 {
		t.Fatalf("unavailable sources = %v; the deployed ClusterRole does not grant what ADR 0032 says it does",
			payload.UnavailableSources)
	}
	if len(payload.Policy.StorageClasses) == 0 {
		t.Fatalf("no storage classes reported, and none declared unavailable: kind ships a default class")
	}

	i, _ := findNS()
	nsPolicy := payload.Policy.Namespaces[i]
	item := nsPolicy.LimitRanges[0].Items[0]
	// This is the number that changes the meaning of workload_metadata: a
	// container declaring nothing gets 5m here, and without the LimitRange a
	// consumer cannot tell that from a team's choice.
	if item.Type != "Container" || item.DefaultRequest.CPUMilli == nil || *item.DefaultRequest.CPUMilli != 5 {
		t.Errorf("limit range item = %+v", item)
	}
	if got := nsPolicy.Quotas[0].Hard["pods"]; got != "50" {
		t.Errorf("quota hard pods = %q", got)
	}
	// kind ships a default StorageClass; the catalog is reported whole, the
	// same standing on which the whole node fleet is reported.
	t.Logf("cluster policy: %d namespaces, %d storage classes (first %q via %q)",
		len(payload.Policy.Namespaces), len(payload.Policy.StorageClasses),
		payload.Policy.StorageClasses[0].Name, payload.Policy.StorageClasses[0].Provisioner)
}

// checkJobRunReported waits for the finished Job to reach a job_runs window
// with its own timings and outcome (ADR 0029).
func checkJobRunReported(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod, fixtureNS string) {
	t.Helper()
	var payload struct {
		Kind    string `json:"kind"`
		Records []struct {
			Namespace  string `json:"namespace"`
			Name       string `json:"name"`
			Result     string `json:"result"`
			StartedAt  string `json:"started_at"`
			FinishedAt string `json:"finished_at"`
			Succeeded  int32  `json:"succeeded"`
		} `json:"records"`
	}
	deadline := time.Now().Add(6 * time.Minute)
	found := false
	for time.Now().Before(deadline) && !found {
		raw, ok := readSpoolGlob(ctx, t, config, cs, ns, pod, jobRunsSpoolGlob)
		if ok && strings.TrimSpace(raw) != "" {
			// The glob may match more than one window — a run near an hour
			// boundary leaves two files, and `cat` concatenates them. Decoding
			// as a stream reads each in turn instead of failing on the second.
			decoder := json.NewDecoder(strings.NewReader(raw))
			for !found {
				if err := decoder.Decode(&payload); err != nil {
					break
				}
				for _, r := range payload.Records {
					if r.Namespace == fixtureNS && r.Name == "nightly" && r.Result != "" {
						found = true
					}
				}
			}
		}
		if !found {
			time.Sleep(5 * time.Second)
		}
	}
	if !found {
		t.Fatalf("no job_runs record for %s/nightly before timeout", fixtureNS)
	}

	if payload.Kind != "job_runs" {
		t.Errorf("kind = %q", payload.Kind)
	}
	for _, r := range payload.Records {
		if r.Namespace != fixtureNS || r.Name != "nightly" {
			continue
		}
		if r.Result != "succeeded" || r.Succeeded != 1 {
			t.Errorf("job run = %+v, want one successful completion", r)
		}
		// A Job carries its own instants, which is why ADR 0029 reports runs
		// that finished before the agent started rather than baselining them.
		if r.StartedAt == "" || r.FinishedAt == "" {
			t.Errorf("job run timings = %+v, want both instants from the object", r)
		}
		t.Logf("job run %s/%s: %s from %s to %s", r.Namespace, r.Name, r.Result, r.StartedAt, r.FinishedAt)
	}
}

// waitForSpoolJSON polls one spool file until it decodes and satisfies ready.
// The payloads are superseding snapshots rewritten every flush, so an early
// read can legitimately show a cluster state that has since moved on; ready is
// what decides the wait is over.
func waitForSpoolJSON(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod, path string, into any, ready func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		raw, ok := readSpoolFile(ctx, t, config, cs, ns, pod, path)
		if ok && strings.TrimSpace(raw) != "" {
			if err := json.Unmarshal([]byte(raw), into); err == nil && ready() {
				return
			}
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("no %s in %s before timeout", what, path)
}

// checkWorkloadShapeReported is the other half of ADR 0048: the two facts that
// ride in `workload_metadata` rather than in the policy payload — the probe
// schedule, without the command the probe runs, and the update the workload
// performs on itself.
func checkWorkloadShapeReported(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod, fixtureNS string) {
	t.Helper()
	var payload struct {
		Kind    string `json:"kind"`
		Records []struct {
			Namespace    string `json:"namespace"`
			WorkloadName string `json:"workload_name"`
			Container    string `json:"container"`
			Probes       struct {
				Liveness *struct {
					Kind             string `json:"kind"`
					PeriodSeconds    int32  `json:"period_seconds"`
					FailureThreshold int32  `json:"failure_threshold"`
				} `json:"liveness"`
			} `json:"probes"`
			Workload struct {
				UpdateStrategy struct {
					Type           string `json:"type"`
					MaxUnavailable string `json:"max_unavailable"`
					MaxSurge       string `json:"max_surge"`
				} `json:"update_strategy"`
			} `json:"workload"`
		} `json:"records"`
	}
	find := func() (int, bool) {
		for i, r := range payload.Records {
			if r.Namespace == fixtureNS && r.WorkloadName == "web" && r.Container == "app" &&
				r.Probes.Liveness != nil {
				return i, true
			}
		}
		return 0, false
	}
	waitForSpoolJSON(ctx, t, config, cs, ns, pod, workloadMetadataPath, &payload, func() bool {
		_, ok := find()
		return ok
	}, "workload metadata carrying the probe schedule")

	if payload.Kind != "workload_metadata" {
		t.Errorf("kind = %q", payload.Kind)
	}
	i, _ := find()
	record := payload.Records[i]

	probe := record.Probes.Liveness
	if probe.Kind != "exec" || probe.PeriodSeconds != 11 || probe.FailureThreshold != 4 {
		t.Errorf("liveness probe = %+v, want the exec schedule the fixture declared", probe)
	}
	strategy := record.Workload.UpdateStrategy
	if strategy.Type != "RollingUpdate" || strategy.MaxUnavailable != "100%" || strategy.MaxSurge != "0" {
		t.Errorf("update strategy = %+v, want the declared 100%%/0 rolling update", strategy)
	}

	// The whole payload, not this record: a probe command must not reach the
	// spool through any field of any kind.
	raw, ok := readSpoolFile(ctx, t, config, cs, ns, pod, workloadMetadataPath)
	if !ok {
		t.Fatal("workload metadata disappeared between the wait and the read")
	}
	if strings.Contains(raw, probeSecret) {
		t.Error("the probe's exec command reached the spool; it must be cleared before the object is cached")
	}
	t.Logf("shape of %s/web: liveness %s every %ds ×%d, update strategy %s %s/%s, and the probe command is absent",
		fixtureNS, probe.Kind, probe.PeriodSeconds, probe.FailureThreshold,
		strategy.Type, strategy.MaxUnavailable, strategy.MaxSurge)
}
