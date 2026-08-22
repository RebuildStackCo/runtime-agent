//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	workloadMetadataSpoolPath = spoolDir + "/workload-metadata.json"
	disruptionsSpoolGlob      = spoolDir + "/disruptions-*.json"
)

// TestPodLifecycleEndToEnd exercises both halves of ADR 0021 in a real cluster.
//
// First: a pod that cannot fit anywhere is reported as unschedulable *with the
// scheduler's reason*, inside the workload-metadata record that already showed
// the replica shortfall.
//
// Second: a low-priority pod is preempted by a high-priority one, and the
// preemption reaches the pod_disruptions payload with the node it was taken
// from. Preemption is used rather than node-pressure eviction because it is the
// one disruption a test can cause deterministically — and it is the same
// capacity story, told by the scheduler instead of the kubelet.
//
// Gated on E2E_AGENT_IMAGE (kind-loaded); use `make lifecycle-e2e`.
func TestPodLifecycleEndToEnd(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	if agentImage == "" {
		t.Skip("E2E_AGENT_IMAGE not set; run `make lifecycle-e2e`")
	}
	config := clusterConfig(t)
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-lifecycle-e2e-%d", os.Getpid())
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		_ = clientset.CoreV1().Namespaces().Delete(cleanupCtx, ns, metav1.DeleteOptions{})
	})

	deployController(ctx, t, clientset, ns, agentImage, renderControllerConfig(ns))
	controllerPod := waitDeploymentPod(ctx, t, clientset, ns, "controller")
	t.Logf("controller pod: %s", controllerPod)

	checkUnschedulableReasonReported(ctx, t, config, clientset, ns, controllerPod)
	checkPreemptionReported(ctx, t, config, clientset, ns, controllerPod)
}

// checkUnschedulableReasonReported creates a pod nothing can fit and waits for
// the workload-metadata record to name the reason.
func checkUnschedulableReasonReported(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, controllerPod string) {
	t.Helper()
	// No node has a thousand cores, so the scheduler reports Unschedulable
	// rather than leaving the pod merely pending.
	deployPods(ctx, t, cs, ns, "toobig", 1, "1000", "", nil)

	deadline := time.Now().Add(4 * time.Minute)
	for {
		if rec, ok := findMetadataRecord(ctx, t, config, cs, ns, controllerPod, "toobig"); ok {
			if rec.Pod.Unscheduled["Unschedulable"] >= 1 {
				t.Logf("unscheduled reasons: %v (replicas %d, nodes %v)",
					rec.Pod.Unscheduled, rec.Pod.Replicas, rec.Pod.Nodes)
				// The reason must explain the shortfall the payload already
				// showed, not contradict it.
				if len(rec.Pod.Nodes) != 0 {
					t.Errorf("nodes = %v, want none for an unschedulable replica", rec.Pod.Nodes)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("workload metadata never reported the unschedulable reason before timeout")
		}
		time.Sleep(5 * time.Second)
	}
}

// checkPreemptionReported fills the node with a low-priority workload, then
// asks for the same room at high priority, and waits for the preemption to
// reach the disruption payload.
func checkPreemptionReported(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, controllerPod string) {
	t.Helper()
	low := createPriorityClass(ctx, t, cs, ns+"-low", 1)
	high := createPriorityClass(ctx, t, cs, ns+"-high", 1000000)

	// Sized against what the node actually has left, so the pair genuinely
	// competes: the victim takes most of the allocatable CPU, and the
	// challenger asks for the same.
	room := schedulableCPU(ctx, t, cs)
	t.Logf("asking %s of CPU for both the victim and the challenger", room.String())

	deployPods(ctx, t, cs, ns, "victim", 1, room.String(), low, nil)
	waitPodPhase(ctx, t, cs, ns, "app=victim", corev1.PodRunning)
	deployPods(ctx, t, cs, ns, "challenger", 1, room.String(), high, nil)

	deadline := time.Now().Add(5 * time.Minute)
	for {
		if rec, ok := findDisruptionRecord(ctx, t, config, cs, ns, controllerPod); ok {
			t.Logf("disruption record: %+v", rec)
			if rec.Reason != "PreemptionByScheduler" {
				t.Errorf("reason = %q, want PreemptionByScheduler", rec.Reason)
			}
			if rec.Workload.Kind != "Deployment" || rec.Workload.Name != "victim" {
				t.Errorf("workload = %s/%s, want Deployment/victim", rec.Workload.Kind, rec.Workload.Name)
			}
			if rec.Node == "" {
				t.Error("node is empty; the node a pod was taken from is the join to node metadata")
			}
			if rec.DisruptedAt.IsZero() {
				t.Error("disrupted_at is zero; the cluster's own timestamp is what places the record in a window")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the pod_disruptions payload never carried the preempted pod before timeout")
		}
		time.Sleep(5 * time.Second)
	}
}

// metadataRecord mirrors the parts of a workload-metadata record this test reads.
type metadataRecord struct {
	WorkloadName string `json:"workload_name"`
	Pod          struct {
		Replicas    int            `json:"replicas"`
		Nodes       map[string]int `json:"nodes"`
		Unscheduled map[string]int `json:"unscheduled"`
	} `json:"pod"`
}

func findMetadataRecord(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod, workload string) (metadataRecord, bool) {
	t.Helper()
	raw, ok := readSpoolFile(ctx, t, config, cs, ns, pod, workloadMetadataSpoolPath)
	if !ok {
		return metadataRecord{}, false
	}
	var payload struct {
		Kind    string           `json:"kind"`
		Records []metadataRecord `json:"records"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Logf("workload metadata not valid JSON yet (will retry): %v", err)
		return metadataRecord{}, false
	}
	for _, r := range payload.Records {
		if r.WorkloadName == workload {
			return r, true
		}
	}
	return metadataRecord{}, false
}

// disruptionRecord mirrors one record of the pod_disruptions payload.
type disruptionRecord struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Workload  struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"workload"`
	Node        string    `json:"node"`
	Reason      string    `json:"reason"`
	DisruptedAt time.Time `json:"disrupted_at"`
}

func findDisruptionRecord(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod string) (disruptionRecord, bool) {
	t.Helper()
	raw, ok := readSpoolGlob(ctx, t, config, cs, ns, pod, disruptionsSpoolGlob)
	if !ok {
		return disruptionRecord{}, false
	}
	var payload struct {
		Kind    string             `json:"kind"`
		Source  string             `json:"source"`
		Records []disruptionRecord `json:"records"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Logf("disruption payload not valid JSON yet (will retry): %v", err)
		return disruptionRecord{}, false
	}
	if payload.Kind != "pod_disruptions" {
		t.Errorf("payload kind = %q, want pod_disruptions", payload.Kind)
	}
	if payload.Source != "journal" {
		t.Errorf("payload source = %q, want journal", payload.Source)
	}
	for _, r := range payload.Records {
		if r.Namespace == ns {
			return r, true
		}
	}
	return disruptionRecord{}, false
}

// schedulableCPU returns a CPU request that one pod can still fit into but two
// cannot: 60% of what is actually *free* on the roomiest node.
//
// Free, not allocatable — the difference is what the first attempt at this test
// got wrong. A kind node's allocatable CPU is already largely spoken for by
// kube-system and by the controller this test just deployed, so a request sized
// against allocatable leaves the victim Pending and there is nothing to preempt.
func schedulableCPU(ctx context.Context, t *testing.T, cs kubernetes.Interface) resource.Quantity {
	t.Helper()
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		t.Fatalf("listing nodes: %v (%d found)", err, len(nodes.Items))
	}
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing pods: %v", err)
	}
	requested := make(map[string]int64)
	for _, p := range pods.Items {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range p.Spec.Containers {
			requested[p.Spec.NodeName] += c.Resources.Requests.Cpu().MilliValue()
		}
	}

	free := int64(0)
	for _, n := range nodes.Items {
		if remaining := n.Status.Allocatable.Cpu().MilliValue() - requested[n.Name]; remaining > free {
			free = remaining
		}
	}
	request := free * 60 / 100
	t.Logf("roomiest node has %dm CPU free; sizing both pods at %dm", free, request)
	if request < 100 {
		t.Fatalf("only %dm of CPU is free cluster-wide, too little to stage a preemption", free)
	}
	return *resource.NewMilliQuantity(request, resource.DecimalSI)
}

// createPriorityClass creates a cluster-scoped PriorityClass and removes it
// after the test. The name carries the namespace so concurrent runs do not
// collide on a cluster-scoped object.
func createPriorityClass(ctx context.Context, t *testing.T, cs kubernetes.Interface, name string, value int32) string {
	t.Helper()
	pc := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Value:      value,
	}
	if _, err := cs.SchedulingV1().PriorityClasses().Create(ctx, pc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating priority class %s: %v", name, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = cs.SchedulingV1().PriorityClasses().Delete(cleanupCtx, name, metav1.DeleteOptions{})
	})
	return name
}

// deployPods creates a Deployment of sleeping containers with the given CPU
// request and priority class. It reuses the spool reader's image, already
// loaded into the cluster.
func deployPods(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name string, replicas int32, cpuRequest, priorityClass string, labels map[string]string) {
	t.Helper()
	if labels == nil {
		labels = map[string]string{"app": name}
	}
	container := corev1.Container{
		Name:            name,
		Image:           spoolReaderImage(),
		ImagePullPolicy: corev1.PullNever,
		Command:         []string{"sh", "-c", "sleep 86400"},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuRequest)},
		},
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers:        []corev1.Container{container},
					PriorityClassName: priorityClass,
				},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating deployment %s: %v", name, err)
	}
	t.Logf("deployment %s created in %s (cpu %s, priority %q)", name, ns, cpuRequest, priorityClass)
}

// waitPodPhase blocks until a pod matching the selector reaches the phase.
func waitPodPhase(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, selector string, phase corev1.PodPhase) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err == nil {
			for _, p := range pods.Items {
				if p.Status.Phase == phase {
					return
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no pod matching %q reached %s before timeout", selector, phase)
}
