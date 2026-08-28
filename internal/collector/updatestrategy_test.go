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

func strategyWatcher(t *testing.T, objects ...runtime.Object) *PodWatcher {
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
func TestUpdateStrategiesReadEveryKindTheAgentKnows(t *testing.T) {
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
	watcher := strategyWatcher(t,
		deployment, statefulSet, daemonSet,
		ownedPod("web-1", "Deployment", "web"),
		ownedPod("db-0", "StatefulSet", "db"),
		ownedPod("logs-xyz", "DaemonSet", "logs"),
	)
	waitFor(t, 5*time.Second, "three strategies", func() bool { return len(watcher.UpdateStrategies()) == 3 })

	got := watcher.UpdateStrategies()
	for key, want := range map[WorkloadKey]UpdateStrategy{
		{Namespace: "shop", Kind: "Deployment", Name: "web"}: {
			Type: "RollingUpdate", MaxUnavailable: "25%", MaxSurge: "1", MinReadySeconds: 30,
		},
		{Namespace: "shop", Kind: "StatefulSet", Name: "db"}: {
			Type: "RollingUpdate", Partition: ptr.To[int32](2),
		},
		{Namespace: "shop", Kind: "DaemonSet", Name: "logs"}: {Type: "OnDelete"},
	} {
		strategy, ok := got[key]
		if !ok {
			t.Errorf("%s/%s has no update strategy", key.Kind, key.Name)
			continue
		}
		if strategy.Type != want.Type || strategy.MaxUnavailable != want.MaxUnavailable ||
			strategy.MaxSurge != want.MaxSurge || strategy.MinReadySeconds != want.MinReadySeconds {
			t.Errorf("%s/%s strategy = %+v, want %+v", key.Kind, key.Name, strategy, want)
		}
		if (strategy.Partition == nil) != (want.Partition == nil) {
			t.Errorf("%s/%s partition = %v, want %v", key.Kind, key.Name, strategy.Partition, want.Partition)
		}
	}
}

// A percentage and a count are not interchangeable when the replica count moves,
// so both are kept as the operator wrote them — the treatment a budget already
// gets (ADR 0032).
func TestUpdateStrategyKeepsPercentAndCountAsWritten(t *testing.T) {
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
	watcher := strategyWatcher(t, deployment, ownedPod("web-1", "Deployment", "web"))
	waitFor(t, 5*time.Second, "the deployment strategy", func() bool { return len(watcher.UpdateStrategies()) == 1 })

	got := watcher.UpdateStrategies()[WorkloadKey{Namespace: "shop", Kind: "Deployment", Name: "web"}]
	if got.MaxUnavailable != "0" || got.MaxSurge != "100%" {
		t.Errorf("maxUnavailable/maxSurge = %q/%q, want \"0\"/\"100%%\"", got.MaxUnavailable, got.MaxSurge)
	}
}

// Every kind the agent does not read is absent, and the agent says nothing more
// about it. A Job replaces no replicas; an Argo Rollout almost certainly does
// and holds its strategy in a custom resource this agent has no RBAC for. The
// agent cannot tell those apart, so it claims neither — which kind a reader is
// looking at is in `workload_kind`, and what that kind implies is a rendering
// (ADR 0004, ADR 0048 §2).
func TestKindsTheAgentDoesNotReadAreAbsentAndUnclaimed(t *testing.T) {
	for _, kind := range []string{"Job", "Rollout", "Node"} {
		watcher := strategyWatcher(t, ownedPod("pod-1", kind, "owner"))
		if got := watcher.UpdateStrategies(); len(got) != 0 {
			t.Errorf("%s: strategies = %v, want none — and no marker standing in for one", kind, got)
		}
	}
}

// The strategy is read when the payload is built, not when a pod was described.
// Editing `maxUnavailable` alone updates nothing, so a value cached at describe
// time would stay wrong until something unrelated happened to the pods.
func TestUpdateStrategyFollowsAnEditThatCausesNoPodEvent(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: appsv1.DeploymentSpec{Strategy: appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: ptr.To(intstr.FromString("25%")),
			},
		}},
	}
	watcher := strategyWatcher(t, deployment, ownedPod("web-1", "Deployment", "web"))
	key := WorkloadKey{Namespace: "shop", Kind: "Deployment", Name: "web"}
	waitFor(t, 5*time.Second, "the declared quarter", func() bool { return watcher.UpdateStrategies()[key].MaxUnavailable == "25%" })

	updated := deployment.DeepCopy()
	updated.Spec.Strategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromString("100%"))
	if _, err := watcher.clientset.AppsV1().Deployments("shop").
		Update(context.Background(), updated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the edit to reach the payload", func() bool { return watcher.UpdateStrategies()[key].MaxUnavailable == "100%" })
}
