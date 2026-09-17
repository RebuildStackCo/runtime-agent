package collector

import (
	"context"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// The list answers the question the counters cannot: which object, and under
// which of the customer's controls. It is read inside the cluster and never
// shipped (ADR 0084), so what it must get right is the reason attached to each
// name — a list that named the wrong control would send someone editing an
// object they never wrote on.

func namespaceObj(name string, annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations}}
}

// decisionsWatcher runs a watcher over the objects and waits until the filter
// has decided about every pod among them.
func decisionsWatcher(t *testing.T, filter *Filter, wantDecided int64, objects ...runtime.Object) *PodWatcher {
	t.Helper()
	clientset := fake.NewClientset(objects...)
	watcher := NewPodWatcher(clientset, func(model.PodInfo) {})
	watcher.SetFilter(filter)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = watcher.Run(ctx) }()

	waitFor(t, 5*time.Second, "the filter to decide about every pod", func() bool {
		c := filter.Snapshot()
		return c.PodsObserved+c.ExcludedNamespaceFilter+c.ExcludedNamespaceAnnotation+
			c.ExcludedWorkloadAnnotation+c.ExcludedPodAnnotation >= wantDecided
	})
	return watcher
}

// find returns the decision about one object, so a test names what it asserts
// about rather than an index into a sorted list.
func find(t *testing.T, list []Decision, kind, namespace, name string) Decision {
	t.Helper()
	for _, d := range list {
		if d.Kind == kind && d.Namespace == namespace && d.Name == name {
			return d
		}
	}
	t.Fatalf("no decision about %s %s/%s in %+v", kind, namespace, name, list)
	return Decision{}
}

func TestEveryControlIsNamedOnTheObjectThatCarriesIt(t *testing.T) {
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web-7d9f", UID: "uid-web-7d9f",
		OwnerReferences: []metav1.OwnerReference{controllerRef("Deployment", "web")},
	}}
	optedOutDeployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web", UID: "uid-web",
		Annotations: map[string]string{CollectAnnotation: "false"},
	}}
	owner := controllerRef("ReplicaSet", "web-7d9f")

	byWorkload := pod("by-workload", &owner)
	byPod := pod("by-pod", nil)
	byPod.Annotations = map[string]string{CollectAnnotation: "false"}
	collected := pod("collected", nil)
	notProfiled := pod("not-profiled", nil)
	notProfiled.Annotations = map[string]string{ProfileAnnotation: "false"}

	inDeniedNamespace := pod("in-denied", nil)
	inDeniedNamespace.Namespace = "kube-system"
	inDeniedNamespace.UID = "uid-in-denied"

	inOptedOutNamespace := pod("in-opted-out", nil)
	inOptedOutNamespace.Namespace = "lab"
	inOptedOutNamespace.UID = "uid-in-opted-out"

	filter := NewFilter(nil, []string{"kube-system"})
	watcher := decisionsWatcher(t, filter, 6,
		namespaceObj("shop", nil),
		namespaceObj("kube-system", nil),
		namespaceObj("lab", map[string]string{CollectAnnotation: "false"}),
		rs, optedOutDeployment,
		byWorkload, byPod, collected, notProfiled, inDeniedNamespace, inOptedOutNamespace,
	)

	list := watcher.Decisions()
	for _, want := range []struct {
		namespace, name string
		collected       bool
		reason          ExclusionReason
	}{
		{"shop", "collected", true, ""},
		{"shop", "not-profiled", true, ""},
		{"shop", "by-workload", false, ExcludedByWorkloadAnnotation},
		{"shop", "by-pod", false, ExcludedByPodAnnotation},
		{"kube-system", "in-denied", false, ExcludedByNamespaceFilter},
		{"lab", "in-opted-out", false, ExcludedByNamespaceAnnotation},
	} {
		got := find(t, list, "Pod", want.namespace, want.name)
		if got.Collected != want.collected || got.Reason != want.reason {
			t.Errorf("%s/%s = collected %v reason %q, want %v and %q",
				want.namespace, want.name, got.Collected, got.Reason, want.collected, want.reason)
		}
	}

	if d := find(t, list, "Pod", "shop", "not-profiled"); d.Profiled {
		t.Error("the pod that refused the profiler is listed as profiled")
	}
	if d := find(t, list, "Pod", "shop", "collected"); !d.Profiled {
		t.Error("a pod that refused nothing is listed as not profiled")
	}
	// An excluded pod is not profiled either, and saying so twice is how a
	// reader learns that the narrower control did not have to be used.
	if d := find(t, list, "Pod", "shop", "by-pod"); d.Profiled {
		t.Error("an excluded pod is listed as profiled")
	}
}

// A namespace nothing runs in still says whether the configuration reaches it.
func TestAnEmptyNamespaceStillStatesItsAnswer(t *testing.T) {
	filter := NewFilter([]string{"shop"}, nil)
	watcher := decisionsWatcher(t, filter, 1,
		namespaceObj("shop", nil),
		namespaceObj("empty", nil),
		pod("collected", nil),
	)

	list := watcher.Decisions()
	if d := find(t, list, "Namespace", "", "empty"); d.Collected || d.Reason != ExcludedByNamespaceFilter {
		t.Errorf("empty namespace = %+v, want excluded by the namespace filter", d)
	}
	if d := find(t, list, "Namespace", "", "shop"); !d.Collected {
		t.Errorf("shop = %+v, want collected", d)
	}
}

// The blind spot ADR 0028 counts is named here as well: this is where a
// customer finds out that their workload annotation could not be read.
func TestAPodWhoseControllerCannotBeReadSaysSo(t *testing.T) {
	owner := controllerRef("Rollout", "web")
	underRollout := pod("under-rollout", &owner)

	filter := NewFilter(nil, nil)
	watcher := decisionsWatcher(t, filter, 1, namespaceObj("shop", nil), underRollout)

	d := find(t, watcher.Decisions(), "Pod", "shop", "under-rollout")
	if !d.Collected {
		t.Errorf("%+v, want collected: an unreadable controller fails open (ADR 0028)", d)
	}
	if d.Workload != WorkloadKindUnknown {
		t.Errorf("workload = %q, want %q", d.Workload, WorkloadKindUnknown)
	}
}

// A Job has a filter step its pods do not — the pod template, which is where an
// opt-out written the pre-ADR-0028 way lives. The list answers for both.
func TestAJobAnswersForItsOwnPayload(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "nightly", UID: "uid-nightly"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{CollectAnnotation: "false"}},
		}},
	}
	filter := NewFilter(nil, nil)
	watcher := decisionsWatcher(t, filter, 0, namespaceObj("shop", nil), job)

	waitFor(t, 5*time.Second, "the Job cache to fill", func() bool {
		for _, d := range watcher.Decisions() {
			if d.Kind == "Job" {
				return true
			}
		}
		return false
	})
	d := find(t, watcher.Decisions(), "Job", "shop", "nightly")
	if d.Collected || d.Reason != ExcludedByObjectAnnotation {
		t.Errorf("%+v, want excluded by the object's own annotation", d)
	}
}

// Answering must not move a number. The counters describe what the agent acted
// on, and reading the list is not one of those decisions (ADR 0054).
func TestReadingTheListCountsNothing(t *testing.T) {
	filter := NewFilter(nil, []string{"kube-system"})
	denied := pod("in-denied", nil)
	denied.Namespace = "kube-system"
	watcher := decisionsWatcher(t, filter, 2,
		namespaceObj("shop", nil), namespaceObj("kube-system", nil),
		pod("collected", nil), denied,
	)

	before := filter.Snapshot()
	for range 3 {
		watcher.Decisions()
	}
	if after := filter.Snapshot(); after != before {
		t.Errorf("coverage moved from %+v to %+v by being read", before, after)
	}
}
