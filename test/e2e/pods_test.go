//go:build e2e

// Package e2e tests the agent against a real Kubernetes cluster. It expects
// a disposable kind cluster and refuses to run against anything else: the
// target context comes only from E2E_KUBE_CONTEXT and must be a kind one.
// Use `make cluster-up e2e cluster-down`.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

const pauseImage = "registry.k8s.io/pause:3.10"

// clusterConfig builds the REST config for the kind context named in
// E2E_KUBE_CONTEXT, refusing any non-kind context. It is the single place the
// e2e resolves a cluster to talk to.
func clusterConfig(t *testing.T) *rest.Config {
	t.Helper()
	contextName := os.Getenv("E2E_KUBE_CONTEXT")
	if contextName == "" {
		t.Skip("E2E_KUBE_CONTEXT is not set; refusing to pick a context implicitly")
	}
	if !strings.HasPrefix(contextName, "kind-") {
		t.Fatalf("E2E_KUBE_CONTEXT=%q: e2e runs only against kind clusters", contextName)
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		t.Fatalf("loading kubeconfig context %q: %v", contextName, err)
	}
	return config
}

func clusterClient(t *testing.T) kubernetes.Interface {
	t.Helper()
	clientset, err := kubernetes.NewForConfig(clusterConfig(t))
	if err != nil {
		t.Fatalf("building clientset: %v", err)
	}
	return clientset
}

func TestPodWatcherAgainstRealCluster(t *testing.T) {
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-e2e-%d", os.Getpid())
	_, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = clientset.CoreV1().Namespaces().Delete(cleanupCtx, ns, metav1.DeleteOptions{})
	})

	// A Deployment: its pods must resolve through the ReplicaSet to it.
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "app", Image: pauseImage,
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
					}},
				},
			},
		},
	}
	if _, err := clientset.AppsV1().Deployments(ns).Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}

	// A suspended CronJob plus a Job owned by it — the same ownership shape
	// the CronJob controller produces, without waiting out a schedule tick.
	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "tick"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 1 1 *",
			Suspend:  ptr.To(true),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: jobSpec(),
			},
		},
	}
	createdCronJob, err := clientset.BatchV1().CronJobs(ns).Create(ctx, cronJob, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating cronjob: %v", err)
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "tick-manual",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "CronJob",
				Name: createdCronJob.Name, UID: createdCronJob.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: jobSpec(),
	}
	if _, err := clientset.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating job: %v", err)
	}

	// Start the watcher only now: both workloads' pods may already exist,
	// which is exactly the list-then-watch path we want to exercise.
	var mu sync.Mutex
	seen := make(map[string]model.PodInfo)
	watcher := collector.NewPodWatcher(clientset, func(p model.PodInfo) {
		observed, _ := json.Marshal(p)
		t.Logf("pod observed: %s", observed)
		if p.Namespace != ns {
			return
		}
		mu.Lock()
		seen[p.Workload.Kind+"/"+p.Workload.Name] = p
		mu.Unlock()
	})
	watcherDone := make(chan error, 1)
	go func() { watcherDone <- watcher.Run(ctx) }()

	waitForWorkload := func(key string) model.PodInfo {
		t.Helper()
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			mu.Lock()
			info, ok := seen[key]
			mu.Unlock()
			if ok {
				return info
			}
			time.Sleep(500 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("no pod reported for workload %s; saw: %v", key, seen)
		return model.PodInfo{}
	}

	// waitForCondition polls the reported view of a workload until pred holds,
	// so assertions can wait for facts that appear on a later status update
	// (the image digest) rather than on the first report.
	waitForCondition := func(key string, pred func(model.PodInfo) bool) model.PodInfo {
		t.Helper()
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			mu.Lock()
			info, ok := seen[key]
			mu.Unlock()
			if ok && pred(info) {
				return info
			}
			time.Sleep(500 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("condition never held for workload %s; last saw: %v", key, seen[key])
		return model.PodInfo{}
	}

	webPod := waitForWorkload("Deployment/web")
	if len(webPod.Containers) != 1 || webPod.Containers[0].Image != pauseImage {
		t.Errorf("deployment pod containers = %+v, want one %s", webPod.Containers, pauseImage)
	}

	// Declared ports are collected verbatim from the spec.
	wantPorts := []model.ContainerPort{{Name: "http", Port: 8080, Protocol: "TCP"}}
	if got := webPod.Containers[0].Ports; !reflect.DeepEqual(got, wantPorts) {
		t.Errorf("deployment pod ports = %+v, want %+v", got, wantPorts)
	}

	// The image digest is only known once the container has started, so it
	// arrives on a later status update — wait for it, then check its shape.
	started := waitForCondition("Deployment/web", func(p model.PodInfo) bool {
		return len(p.Containers) == 1 && p.Containers[0].ImageDigest != ""
	})
	if d := started.Containers[0].ImageDigest; !strings.HasPrefix(d, "sha256:") {
		t.Errorf("deployment pod image digest = %q, want a sha256: digest", d)
	}

	res := webPod.Containers[0].Resources
	wantRes := map[string]struct {
		got  *int64
		want int64
	}{
		"cpu_request_milli":    {res.CPURequestMilli, 50},
		"cpu_limit_milli":      {res.CPULimitMilli, 100},
		"memory_request_bytes": {res.MemoryRequestBytes, 32 << 20},
		"memory_limit_bytes":   {res.MemoryLimitBytes, 64 << 20},
	}
	for field, v := range wantRes {
		if v.got == nil || *v.got != v.want {
			t.Errorf("deployment pod %s = %v, want %d", field, v.got, v.want)
		}
	}
	if webPod.QOSClass != string(corev1.PodQOSBurstable) {
		t.Errorf("deployment pod qos = %q, want Burstable", webPod.QOSClass)
	}

	waitForWorkload("CronJob/tick")

	// Subscription: a pod created while the watcher is running is reported.
	barePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "debug-shell"},
		Spec: corev1.PodSpec{
			Containers:    []corev1.Container{{Name: "shell", Image: pauseImage}},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}
	if _, err := clientset.CoreV1().Pods(ns).Create(ctx, barePod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating bare pod: %v", err)
	}
	bare := waitForWorkload("none/")
	if bare.Name != "debug-shell" {
		t.Errorf("bare pod name = %q, want debug-shell", bare.Name)
	}

	cancel()
	if err := <-watcherDone; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
}

func jobSpec() batchv1.JobSpec {
	return batchv1.JobSpec{
		BackoffLimit: ptr.To(int32(0)),
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "tick", Image: pauseImage}},
			},
		},
	}
}
