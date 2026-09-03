package collector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

var rsCreated = time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)

func replicaSet(name, deployment, revision, image string, desired, ready int32) *appsv1.ReplicaSet {
	set := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: name, UID: types.UID("uid-" + name),
			CreationTimestamp: metav1.Time{Time: rsCreated},
			OwnerReferences:   []metav1.OwnerReference{controllerRef("Deployment", deployment)},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: ptr.To(desired),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{
					Name: "migrate", Image: "example.com/migrate:v3",
					// Never read, and the test says so by putting something
					// here that must not appear in any collected view.
					Env: []corev1.EnvVar{{Name: "DB_PASSWORD", Value: "hunter2"}},
				}},
				Containers: []corev1.Container{{
					Name: "app", Image: image,
					Command: []string{"/app", "--token=secret"},
				}},
			}},
		},
		Status: appsv1.ReplicaSetStatus{Replicas: desired, ReadyReplicas: ready},
	}
	if revision != "" {
		set.Annotations = map[string]string{revisionAnnotation: revision}
	}
	return set
}

func collectReplicaSets(t *testing.T, objects ...runtime.Object) []model.ReplicaSetInfo {
	t.Helper()
	clientset := fake.NewClientset(objects...)
	watcher := NewPodWatcher(clientset, func(model.PodInfo) {})
	watcher.SetFilter(NewFilter(nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	var got []model.ReplicaSetInfo
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got = watcher.ReplicaSets(); len(got) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
	return got
}

// The revision view carries the object's numbers and its images, and nothing
// else from the pod template. `env` and `command` sit in the same template and
// must never be collected (CLAUDE.md invariant 4).
func TestRevisionViewCarriesImagesAndNoTemplateSecrets(t *testing.T) {
	owner := controllerRef("ReplicaSet", "web-old")
	got := collectReplicaSets(t,
		replicaSet("web-old", "web", "46", "example.com/app:1.2.3", 3, 3),
		pod("web-pod", &owner),
	)
	if len(got) != 1 {
		t.Fatalf("collected %d revisions, want 1", len(got))
	}
	rev := got[0]
	if rev.Revision == nil || *rev.Revision != 46 {
		t.Errorf("revision = %v, want 46", rev.Revision)
	}
	if rev.Workload.Kind != "Deployment" || rev.Workload.Name != "web" {
		t.Errorf("workload = %+v, want the owning Deployment", rev.Workload)
	}
	if rev.DesiredReplicas != 3 || rev.ReadyReplicas != 3 {
		t.Errorf("replicas = (%d desired, %d ready), want (3, 3)", rev.DesiredReplicas, rev.ReadyReplicas)
	}
	if !rev.CreatedAt.Equal(rsCreated) {
		t.Errorf("created_at = %s, want %s", rev.CreatedAt, rsCreated)
	}
	if len(rev.Containers) != 2 {
		t.Fatalf("collected %d containers, want 2 (the init container counts: its image is part of the revision)", len(rev.Containers))
	}
	if !rev.Containers[0].Init || rev.Containers[0].Name != "migrate" {
		t.Errorf("first container = %+v, want the init container marked", rev.Containers[0])
	}
	if rev.Containers[1].Image != "example.com/app:1.2.3" {
		t.Errorf("image = %q, want the template's", rev.Containers[1].Image)
	}
}

// The revision set is inherited from the admitted pod index, not decided again.
// A Deployment with no admitted pods has no revisions here even while its
// ReplicaSets exist — the same forgetting ADR 0018 chose for the Go inventory.
func TestRevisionsCoverOnlyDeploymentsWithAdmittedPods(t *testing.T) {
	owner := controllerRef("ReplicaSet", "web-old")
	got := collectReplicaSets(t,
		replicaSet("web-old", "web", "46", "example.com/app:1.2.3", 3, 3),
		replicaSet("ghost-old", "ghost", "12", "example.com/ghost:9", 0, 0),
		pod("web-pod", &owner),
	)
	for _, rev := range got {
		if rev.Workload.Name == "ghost" {
			t.Errorf("reported a revision of a Deployment with no admitted pods: %+v", rev)
		}
	}
	if len(got) != 1 {
		t.Fatalf("collected %d revisions, want only web's", len(got))
	}
}

// An opted-out Deployment has no admitted pods, so it has no revisions either —
// the exclusion reaches this payload without a filter call of its own
// (ADR 0028, ADR 0030).
func TestOptedOutDeploymentHasNoRevisions(t *testing.T) {
	owner := controllerRef("ReplicaSet", "web-old")
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web",
		Annotations: map[string]string{CollectAnnotation: "false"},
	}}
	clientset := fake.NewClientset(deployment,
		replicaSet("web-old", "web", "46", "example.com/app:1.2.3", 3, 3),
		pod("web-pod", &owner))

	watcher := NewPodWatcher(clientset, func(model.PodInfo) {})
	filter := NewFilter(nil, nil)
	watcher.SetFilter(filter)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if filter.Snapshot().ExcludedWorkloadAnnotation > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := watcher.ReplicaSets()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("collected %d revisions of an opted-out Deployment: %+v", len(got), got)
	}
}

// A ReplicaSet the Deployment controller never annotated has no revision, and
// that is reported as absent rather than invented as zero.
func TestUnannotatedReplicaSetHasNoRevision(t *testing.T) {
	owner := controllerRef("ReplicaSet", "web-old")
	got := collectReplicaSets(t,
		replicaSet("web-old", "web", "", "example.com/app:1.2.3", 1, 1),
		pod("web-pod", &owner),
	)
	if len(got) != 1 {
		t.Fatalf("collected %d revisions, want 1", len(got))
	}
	if got[0].Revision != nil {
		t.Errorf("revision = %d, want absent", *got[0].Revision)
	}
}

// The reduction is where a leak would be introduced, so this checks the bytes
// rather than the shape: a ReplicaSet whose template carries an inline
// credential in `env` and another in `command` must produce a view containing
// neither. A future field added to ReplicaSetInfo that carries the template
// through fails here rather than in a customer's cluster (CLAUDE.md invariant 4).
func TestRevisionViewNeverCarriesTemplateEnvOrCommand(t *testing.T) {
	set := replicaSet("web-old", "web", "46", "example.com/app:1.2.3", 3, 3)
	encoded, err := json.Marshal(describeReplicaSet(set, model.WorkloadKey{Namespace: "shop", Kind: "Deployment", Name: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"hunter2", "DB_PASSWORD", "--token=secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("the collected revision view carries %q: %s", forbidden, encoded)
		}
	}
}

// replicaSetUnderKind is the same fixture under an arbitrary controller kind, with
// that controller's own revision annotation rather than the Deployment one.
func replicaSetUnderKind(name, kind, owner string) *appsv1.ReplicaSet {
	set := replicaSet(name, owner, "", "example.com/payments:2.0.1", 2, 2)
	set.OwnerReferences = []metav1.OwnerReference{controllerRef(kind, owner)}
	set.Annotations = map[string]string{"made.up.io/revision": "7"}
	return set
}

// The kind of the controller was never what made a ReplicaSet worth reporting —
// only knowing the name of its revision annotation was. An Argo Rollout creates
// and scales ordinary ReplicaSets, so its revisions are collected like any other
// (ADR 0049 §1). The number is not: that controller writes it under a key of its
// own, and guessing at keys is what §3 declines.
func TestRevisionsCoverAnyControllerOfAReplicaSet(t *testing.T) {
	owner := controllerRef("ReplicaSet", "payments-abc")
	got := collectReplicaSets(t,
		replicaSetUnderKind("payments-abc", "Rollout", "payments"),
		pod("payments-pod", &owner),
	)
	if len(got) != 1 {
		t.Fatalf("collected %d revisions, want the Rollout's one: %+v", len(got), got)
	}
	rev := got[0]
	if rev.Workload.Kind != "Rollout" || rev.Workload.Name != "payments" {
		t.Errorf("workload = %+v, want the Rollout that owns the set", rev.Workload)
	}
	if rev.Revision != nil {
		t.Errorf("revision = %d, want absent: the key is the controller's, not ours", *rev.Revision)
	}
	if rev.CreatedAt.IsZero() || len(rev.Containers) == 0 {
		t.Errorf("the record lost what orders and identifies it: %+v", rev)
	}
}

// A ReplicaSet nothing controls is the workload, not a revision of one. Its pods
// resolve to the set itself, so reporting it here would file a workload as a
// version of nothing (ADR 0049 §2).
func TestABareReplicaSetIsNotARevision(t *testing.T) {
	bare := replicaSet("standalone", "", "", "example.com/app:1.0.0", 1, 1)
	bare.OwnerReferences = nil
	owner := controllerRef("ReplicaSet", "standalone")
	if got := collectReplicaSets(t, bare, pod("standalone-pod", &owner)); len(got) != 0 {
		t.Errorf("collected %+v, want nothing for a ReplicaSet with no controller", got)
	}
}
