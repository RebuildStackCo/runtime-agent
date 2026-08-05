package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			allowed, reason := NewPodFilter(c.allow, c.deny).Admit(c.pod, c.nsAnn)
			if allowed != c.wantAllowed || reason != c.wantReason {
				t.Errorf("Admit = (%v, %q), want (%v, %q)", allowed, reason, c.wantAllowed, c.wantReason)
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
	seen := make(map[string]PodInfo)
	watcher := NewPodWatcher(clientset, func(p PodInfo) {
		mu.Lock()
		seen[p.Namespace+"/"+p.Name] = p
		mu.Unlock()
	})
	filter := NewPodFilter(nil, []string{"pr-*"})
	watcher.SetFilter(filter)
	oomEvents := make(chan OOMKill, 16)
	watcher.OnOOMKill(func(o OOMKill) { oomEvents <- o })

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

	want := Coverage{
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
