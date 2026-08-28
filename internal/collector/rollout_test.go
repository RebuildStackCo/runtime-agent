package collector

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

// ownedPod is a pod of the named controller, on a node and running, so the
// admitted index holds it.
func ownedPod(name, kind, owner string) *corev1.Pod {
	p := pod(name, &metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: kind, Name: owner, Controller: ptr.To(true),
	})
	return p
}

func rolloutWatcher(t *testing.T, objects ...runtime.Object) *PodWatcher {
	t.Helper()
	watcher := NewPodWatcher(fake.NewClientset(objects...), func(PodInfo) {})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = watcher.Run(ctx) }()
	waitFor(t, 5*time.Second, "the pod index to fill", func() bool { return len(watcher.Pods()) > 0 })
	return watcher
}

// Each kind states its strategy in its own field, and the reduction has to reach
// all three: a Deployment's `strategy`, a StatefulSet's and a DaemonSet's
// `updateStrategy` (ADR 0048 §2).
func TestRolloutsReadEveryKindThatHasAStrategy(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: appsv1.DeploymentSpec{
			MinReadySeconds: 30,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: ptr.To(intstr.FromString("25%")),
					MaxSurge:       ptr.To(intstr.FromInt32(1)),
				},
			},
		},
	}
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db"},
		Spec: appsv1.StatefulSetSpec{
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](2)},
			},
		},
	}
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "logs"},
		Spec: appsv1.DaemonSetSpec{
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType},
		},
	}
	watcher := rolloutWatcher(t,
		deployment, statefulSet, daemonSet,
		ownedPod("web-1", "Deployment", "web"),
		ownedPod("db-0", "StatefulSet", "db"),
		ownedPod("logs-xyz", "DaemonSet", "logs"),
	)
	waitFor(t, 5*time.Second, "three rollouts", func() bool { return len(watcher.Rollouts()) == 3 })

	got := watcher.Rollouts()
	for key, want := range map[WorkloadKey]Rollout{
		{Namespace: "shop", Kind: "Deployment", Name: "web"}: {
			Type: "RollingUpdate", MaxUnavailable: "25%", MaxSurge: "1", MinReadySeconds: 30,
		},
		{Namespace: "shop", Kind: "StatefulSet", Name: "db"}: {
			Type: "RollingUpdate", Partition: ptr.To[int32](2),
		},
		{Namespace: "shop", Kind: "DaemonSet", Name: "logs"}: {Type: "OnDelete"},
	} {
		rollout, ok := got[key]
		if !ok {
			t.Errorf("%s/%s has no rollout", key.Kind, key.Name)
			continue
		}
		if rollout.Type != want.Type || rollout.MaxUnavailable != want.MaxUnavailable ||
			rollout.MaxSurge != want.MaxSurge || rollout.MinReadySeconds != want.MinReadySeconds {
			t.Errorf("%s/%s rollout = %+v, want %+v", key.Kind, key.Name, rollout, want)
		}
		if (rollout.Partition == nil) != (want.Partition == nil) {
			t.Errorf("%s/%s partition = %v, want %v", key.Kind, key.Name, rollout.Partition, want.Partition)
		}
	}
}

// A percentage and a count are not interchangeable when the replica count moves,
// so both are kept as the operator wrote them — the treatment a budget already
// gets (ADR 0032).
func TestRolloutKeepsPercentAndCountAsWritten(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: appsv1.DeploymentSpec{Strategy: appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: ptr.To(intstr.FromInt32(0)),
				MaxSurge:       ptr.To(intstr.FromString("100%")),
			},
		}},
	}
	watcher := rolloutWatcher(t, deployment, ownedPod("web-1", "Deployment", "web"))
	waitFor(t, 5*time.Second, "the deployment rollout", func() bool { return len(watcher.Rollouts()) == 1 })

	got := watcher.Rollouts()[WorkloadKey{Namespace: "shop", Kind: "Deployment", Name: "web"}]
	if got.MaxUnavailable != "0" || got.MaxSurge != "100%" {
		t.Errorf("maxUnavailable/maxSurge = %q/%q, want \"0\"/\"100%%\"", got.MaxUnavailable, got.MaxSurge)
	}
}

// A kind with no update strategy is absent rather than present and empty: a
// bare Job does not roll, and reporting a zero rollout for it would read as one
// that takes everything down at once.
func TestRolloutsAreAbsentForKindsThatDoNotRoll(t *testing.T) {
	watcher := rolloutWatcher(t, ownedPod("nightly-1", "Job", "nightly"))
	if got := watcher.Rollouts(); len(got) != 0 {
		t.Errorf("rollouts = %v, want none for a Job's pod", got)
	}
}

// An Argo Rollout is the case absence would answer wrongly. It has an update
// strategy; the agent holds no RBAC to read it. Reporting nothing would say
// "this does not roll", which is the opposite of true — so it says it did not
// look (ADR 0048 §2).
func TestAControllerTheAgentCannotReadSaysSoRatherThanNothing(t *testing.T) {
	watcher := rolloutWatcher(t, ownedPod("payments-1", "Rollout", "payments"))
	key := WorkloadKey{Namespace: "shop", Kind: "Rollout", Name: "payments"}
	waitFor(t, 5*time.Second, "the unread marker", func() bool {
		_, ok := watcher.Rollouts()[key]
		return ok
	})

	got := watcher.Rollouts()[key]
	if !got.Unread {
		t.Errorf("rollout = %+v, want it marked unread", got)
	}
	if got.Type != "" || got.MaxUnavailable != "" || got.MaxSurge != "" || got.MinReadySeconds != 0 {
		t.Errorf("rollout = %+v; an unread controller must state nothing else", got)
	}
}

// The strategy is read when the payload is built, not when a pod was described.
// Editing `maxUnavailable` alone rolls nothing, so a value cached at describe
// time would stay wrong until something unrelated happened to the pods.
func TestRolloutFollowsAnEditThatCausesNoPodEvent(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: appsv1.DeploymentSpec{Strategy: appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: ptr.To(intstr.FromString("25%")),
			},
		}},
	}
	watcher := rolloutWatcher(t, deployment, ownedPod("web-1", "Deployment", "web"))
	key := WorkloadKey{Namespace: "shop", Kind: "Deployment", Name: "web"}
	waitFor(t, 5*time.Second, "the declared quarter", func() bool { return watcher.Rollouts()[key].MaxUnavailable == "25%" })

	updated := deployment.DeepCopy()
	updated.Spec.Strategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromString("100%"))
	if _, err := watcher.clientset.AppsV1().Deployments("shop").
		Update(context.Background(), updated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the edit to reach the payload", func() bool { return watcher.Rollouts()[key].MaxUnavailable == "100%" })
}
