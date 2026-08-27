package collector

import (
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// lastAppliedAnnotation holds a verbatim copy of the object as applied, env
// and all, so stripping a container's env without stripping this would move
// the values rather than drop them.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// dropUncollectedFields is where "filter early" is enforced for everything the
// controller watches: the informer cache is the source, and a field removed
// here is never held, not merely never shipped (CLAUDE.md invariant 4,
// ADR 0046).
//
// It runs on every object entering every cache, so a kind it does not name
// still loses its managed fields and its applied-configuration copy.
func dropUncollectedFields(obj any) (any, error) {
	if m, err := meta.Accessor(obj); err == nil {
		m.SetManagedFields(nil)
		delete(m.GetAnnotations(), lastAppliedAnnotation)
	}
	switch o := obj.(type) {
	case *corev1.Pod:
		dropPodSpecFields(&o.Spec)
	case *appsv1.Deployment:
		dropPodSpecFields(&o.Spec.Template.Spec)
	case *appsv1.StatefulSet:
		dropPodSpecFields(&o.Spec.Template.Spec)
	case *appsv1.DaemonSet:
		dropPodSpecFields(&o.Spec.Template.Spec)
	case *appsv1.ReplicaSet:
		dropPodSpecFields(&o.Spec.Template.Spec)
	case *batchv1.Job:
		dropPodSpecFields(&o.Spec.Template.Spec)
	case *batchv1.CronJob:
		dropPodSpecFields(&o.Spec.JobTemplate.Spec.Template.Spec)
	}
	return obj, nil
}

// dropPodSpecFields clears the three fields the agent promises never to
// collect, in every container list a pod spec has.
func dropPodSpecFields(spec *corev1.PodSpec) {
	for i := range spec.Containers {
		dropContainerFields(&spec.Containers[i])
	}
	for i := range spec.InitContainers {
		dropContainerFields(&spec.InitContainers[i])
	}
	for i := range spec.EphemeralContainers {
		dropContainerFields((*corev1.Container)(&spec.EphemeralContainers[i].EphemeralContainerCommon))
	}
}

func dropContainerFields(c *corev1.Container) {
	c.Env = nil
	c.EnvFrom = nil
	c.Command = nil
	c.Args = nil
}
