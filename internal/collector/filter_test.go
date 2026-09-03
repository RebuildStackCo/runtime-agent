package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"shop", "shop", true},
		{"shop", "shop-2", false},
		{"*", "anything", true},
		{"*", "", true},
		{"team-*", "team-ml", true},
		{"team-*", "team-", true},
		{"team-*", "steam-ml", false},
		{"*-prod", "shop-prod", true},
		{"*-prod", "prod", false},
		{"pr-*-preview", "pr-1234-preview", true},
		{"pr-*-preview", "pr-preview", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "acb", false},
		{"a*a", "a", false},
		{"", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.s); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestAdmitSemantics(t *testing.T) {
	podIn := func(namespace string, annotations map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: "p", Annotations: annotations,
		}}
	}
	optOut := map[string]string{CollectAnnotation: "false"}

	cases := []struct {
		name        string
		allow, deny []string
		pod         *corev1.Pod
		nsAnn       map[string]string
		workload    WorkloadLookup
		wantAllowed bool
		wantReason  ExclusionReason
	}{
		{name: "no rules admit everything", pod: podIn("anything", nil), wantAllowed: true},
		{name: "allow admits match", allow: []string{"shop", "team-*"}, pod: podIn("team-ml", nil), wantAllowed: true},
		{name: "allow rejects non-match", allow: []string{"shop"}, pod: podIn("payments", nil), wantReason: ExcludedByNamespaceFilter},
		{name: "deny rejects with empty allow", deny: []string{"pr-*"}, pod: podIn("pr-1234", nil), wantReason: ExcludedByNamespaceFilter},
		{name: "deny wins over allow", allow: []string{"team-*"}, deny: []string{"team-sandbox"}, pod: podIn("team-sandbox", nil), wantReason: ExcludedByNamespaceFilter},
		{name: "namespace annotation opts out", pod: podIn("shop", nil), nsAnn: optOut, wantReason: ExcludedByNamespaceAnnotation},
		{name: "pod annotation opts out", pod: podIn("shop", optOut), wantReason: ExcludedByPodAnnotation},
		{name: "other annotation value collects", pod: podIn("shop", map[string]string{CollectAnnotation: "yes"}), wantAllowed: true},
		{name: "namespace filter attributed before annotations", deny: []string{"shop"}, pod: podIn("shop", optOut), nsAnn: optOut, wantReason: ExcludedByNamespaceFilter},
		{name: "namespace annotation attributed before pod annotation", pod: podIn("shop", optOut), nsAnn: optOut, wantReason: ExcludedByNamespaceAnnotation},

		// The workload step. It sits between the namespace and the pod, and it
		// is the only one that does not reject when it cannot be evaluated.
		{name: "workload annotation opts out", pod: podIn("shop", nil), workload: WorkloadLookup{Annotations: optOut}, wantReason: ExcludedByWorkloadAnnotation},
		{name: "workload annotation attributed before pod annotation", pod: podIn("shop", optOut), workload: WorkloadLookup{Annotations: optOut}, wantReason: ExcludedByWorkloadAnnotation},
		{name: "namespace annotation attributed before workload annotation", pod: podIn("shop", nil), nsAnn: optOut, workload: WorkloadLookup{Annotations: optOut}, wantReason: ExcludedByNamespaceAnnotation},
		{name: "other workload annotation value collects", pod: podIn("shop", nil), workload: WorkloadLookup{Annotations: map[string]string{CollectAnnotation: "yes"}}, wantAllowed: true},
		// Fail-open, deliberately: this product is opt-out, so a controller the
		// agent cannot read is not evidence that anyone opted out (ADR 0028).
		{name: "unknown workload kind still collects", pod: podIn("shop", nil), workload: WorkloadLookup{Unresolved: WorkloadKindUnknown}, wantAllowed: true},
		{name: "uncached workload still collects", pod: podIn("shop", nil), workload: WorkloadLookup{Unresolved: WorkloadNotCached}, wantAllowed: true},
		// ...but a control the customer did set still applies to it.
		{name: "unreadable workload does not override the pod's own opt-out", pod: podIn("shop", optOut), workload: WorkloadLookup{Unresolved: WorkloadKindUnknown}, wantReason: ExcludedByPodAnnotation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			allowed, reason := NewFilter(c.allow, c.deny).AdmitPod(c.pod, c.nsAnn, c.workload)
			if allowed != c.wantAllowed || reason != c.wantReason {
				t.Errorf("AdmitPod = (%v, %q), want (%v, %q)", allowed, reason, c.wantAllowed, c.wantReason)
			}
		})
	}
}

func TestWatcherGatesPodsAndCountsCoverage(t *testing.T) {
	optOutNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "ml-experiments", Annotations: map[string]string{CollectAnnotation: "false"},
	}}
	deniedPod := pod("in-denied-ns", nil)
	deniedPod.Namespace = "pr-1234"
	optedOutPod := pod("in-opted-ns", nil)
	optedOutPod.Namespace = "ml-experiments"
	annotatedPod := pod("self-opted", nil)
	annotatedPod.Annotations = map[string]string{CollectAnnotation: "false"}
	collected := pod("collected", nil)

	clientset := fake.NewClientset(optOutNs, deniedPod, optedOutPod, annotatedPod, collected)

	var mu sync.Mutex
	seen := make(map[string]model.PodInfo)
	watcher := NewPodWatcher(clientset, func(p model.PodInfo) {
		mu.Lock()
		seen[p.Namespace+"/"+p.Name] = p
		mu.Unlock()
	})
	filter := NewFilter(nil, []string{"pr-*"})
	watcher.SetFilter(filter)
	oomEvents := make(chan model.OOMKill, 16)
	watcher.OnOOMKill(func(o model.OOMKill) { oomEvents <- o })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	// Wait until every pod's Add went through the filter, then check who
	// got reported.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := filter.Snapshot()
		if c.PodsObserved+c.ExcludedNamespaceFilter+c.ExcludedNamespaceAnnotation+c.ExcludedPodAnnotation >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("filter saw %+v before timeout, want 4 pods total", c)
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	if len(seen) != 1 {
		t.Errorf("reported pods = %v, want only shop/collected", seen)
	}
	if _, ok := seen["shop/collected"]; !ok {
		t.Errorf("shop/collected was not reported; saw %v", seen)
	}
	mu.Unlock()

	want := model.Coverage{
		PodsObserved:                1,
		ExcludedNamespaceFilter:     1,
		ExcludedNamespaceAnnotation: 1,
		ExcludedPodAnnotation:       1,
	}
	if got := filter.Snapshot(); got != want {
		t.Errorf("coverage = %+v, want %+v", got, want)
	}

	// An excluded pod's OOM kill must be gated too: update the denied pod
	// with an OOM, then the collected pod. Receiving only the collected
	// pod's kill proves the denied one was suppressed.
	killedAt := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	oomStatus := func(p *corev1.Pod, at time.Time) *corev1.Pod {
		update := p.DeepCopy()
		update.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:                 "app",
			RestartCount:         1,
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			LastTerminationState: terminated(oomKilledReason, 137, at),
		}}
		return update
	}
	if _, err := clientset.CoreV1().Pods("pr-1234").Update(ctx, oomStatus(deniedPod, killedAt), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating denied pod: %v", err)
	}
	if _, err := clientset.CoreV1().Pods("shop").Update(ctx, oomStatus(collected, killedAt), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating collected pod: %v", err)
	}
	select {
	case o := <-oomEvents:
		if o.Namespace != "shop" || o.Pod != "collected" {
			t.Errorf("OOM kill reported for %s/%s, want shop/collected (excluded pod leaked)", o.Namespace, o.Pod)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no OOM kill reported before timeout")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
}

// The opt-out annotation on the controller itself, walked through the real
// owner chain. This is the control that did not exist before ADR 0028: the
// only way to exclude a workload used to be an annotation in its pod template,
// which is part of the template hash and therefore rolls every replica.
//
// The Rollout case is the blind spot, and it is deliberately a collection, not
// an exclusion: the agent cannot read a CRD, and an unreadable controller is
// not evidence that anyone opted out.
func TestWorkloadAnnotationExcludesWithoutTouchingPodTemplates(t *testing.T) {
	optOut := map[string]string{CollectAnnotation: "false"}
	meta := func(name string, ann map[string]string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: "shop", Name: name, Annotations: ann,
			UID: types.UID("uid-" + name)}
	}
	owned := func(podName string, owner metav1.OwnerReference) *corev1.Pod {
		p := pod(podName, &owner)
		return p
	}

	// A Deployment that opted out, reached through its ReplicaSet.
	deployment := &appsv1.Deployment{ObjectMeta: meta("web", optOut)}
	webRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web-abc", UID: "uid-web-abc",
		OwnerReferences: []metav1.OwnerReference{controllerRef("Deployment", "web")},
	}}
	// A CronJob that opted out, reached through its Job.
	cron := &batchv1.CronJob{ObjectMeta: meta("report", optOut)}
	reportJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "report-1", UID: "uid-report-1",
		OwnerReferences: []metav1.OwnerReference{controllerRef("CronJob", "report")},
	}}
	// A StatefulSet that did not opt out.
	sts := &appsv1.StatefulSet{ObjectMeta: meta("index", nil)}
	// A ReplicaSet owned by a CRD: unreadable, therefore collected and counted.
	rolloutRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "pay-xyz", UID: "uid-pay-xyz",
		OwnerReferences: []metav1.OwnerReference{controllerRef("Rollout", "payments")},
	}}

	clientset := fake.NewClientset(
		deployment, webRS, cron, reportJob, sts, rolloutRS,
		owned("web-pod", controllerRef("ReplicaSet", "web-abc")),
		owned("report-pod", controllerRef("Job", "report-1")),
		owned("index-pod", controllerRef("StatefulSet", "index")),
		owned("pay-pod", controllerRef("ReplicaSet", "pay-xyz")),
	)

	var mu sync.Mutex
	seen := map[string]bool{}
	watcher := NewPodWatcher(clientset, func(p model.PodInfo) {
		mu.Lock()
		seen[p.Name] = true
		mu.Unlock()
	})
	filter := NewFilter(nil, nil)
	watcher.SetFilter(filter)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		c := filter.Snapshot()
		if c.PodsObserved+c.ExcludedWorkloadAnnotation == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("filter saw %+v before timeout, want 4 pods total", c)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"index-pod", "pay-pod"} {
		if !seen[name] {
			t.Errorf("%s was not reported; the workload did not opt out", name)
		}
	}
	for _, name := range []string{"web-pod", "report-pod"} {
		if seen[name] {
			t.Errorf("%s was reported; its workload carries %s=false", name, CollectAnnotation)
		}
	}

	got := filter.Snapshot()
	if got.ExcludedWorkloadAnnotation != 2 {
		t.Errorf("excluded by workload annotation = %d, want 2 (the Deployment and the CronJob)",
			got.ExcludedWorkloadAnnotation)
	}
	if got.PodsObserved != 2 {
		t.Errorf("pods observed = %d, want 2", got.PodsObserved)
	}
	if got.WorkloadUnknownKind != 1 {
		t.Errorf("workload_unknown_kind = %d, want 1 (the Rollout-owned ReplicaSet); "+
			"this counter is what tells a customer the workload-level control does not "+
			"reach their operator's workloads", got.WorkloadUnknownKind)
	}
	if got.ExcludedPodAnnotation != 0 || got.ExcludedNamespaceAnnotation != 0 {
		t.Errorf("unexpected exclusions %+v; no pod or namespace was annotated", got)
	}
}
