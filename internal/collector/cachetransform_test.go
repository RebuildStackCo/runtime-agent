package collector

import (
	"context"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// secretish is what makes this an invariant and not housekeeping: env values
// are where connection strings and tokens are written by hand.
// #nosec G101 -- a fixture, and the scanner reading it that way is the point
const secretish = "postgres://svc:hunter2@db.internal/prod"

func loadedContainer(name string) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   "example.com/app:1.2.3",
		Command: []string{"/bin/sh", "-c"},
		Args:    []string{"exec /app --token=t0ps3cret"},
		Env:     []corev1.EnvVar{{Name: "DATABASE_URL", Value: secretish}},
		EnvFrom: []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}},
		}},
	}
}

func loadedTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: []corev1.Container{loadedContainer("app")}},
	}
}

func loadedMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Namespace: "shop",
		Name:      name,
		Annotations: map[string]string{
			lastAppliedAnnotation: `{"spec":{"containers":[{"env":[{"name":"DATABASE_URL","value":"` + secretish + `"}]}]}}`,
			CollectAnnotation:     "false",
		},
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}},
	}
}

// containersIn returns every container an object holds, wherever the API
// buries it: a pod's own three lists, a workload's template, a CronJob's
// template of a template.
func containersIn(obj any) []corev1.Container {
	var out []corev1.Container
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		case reflect.Struct:
			switch c := v.Interface().(type) {
			case corev1.Container:
				out = append(out, c)
				return
			case corev1.EphemeralContainer:
				out = append(out, corev1.Container(c.EphemeralContainerCommon))
				return
			}
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		}
	}
	walk(reflect.ValueOf(obj))
	return out
}

// "Not collected" beats "collected and dropped": the cache is the source, so
// the fields have to go before the object is stored, not before it is
// serialized (CLAUDE.md invariant 4, ADR 0046).
func TestNoWatchedKindEntersTheCacheCarryingEnvArgsOrCommand(t *testing.T) {
	podWithEphemeral := &corev1.Pod{ObjectMeta: loadedMeta("web")}
	podWithEphemeral.Spec.Containers = []corev1.Container{loadedContainer("app")}
	podWithEphemeral.Spec.InitContainers = []corev1.Container{loadedContainer("init")}
	podWithEphemeral.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon(loadedContainer("debug")),
	}}

	objects := []any{
		podWithEphemeral,
		&appsv1.Deployment{ObjectMeta: loadedMeta("web"), Spec: appsv1.DeploymentSpec{Template: loadedTemplate()}},
		&appsv1.StatefulSet{ObjectMeta: loadedMeta("db"), Spec: appsv1.StatefulSetSpec{Template: loadedTemplate()}},
		&appsv1.DaemonSet{ObjectMeta: loadedMeta("logs"), Spec: appsv1.DaemonSetSpec{Template: loadedTemplate()}},
		&appsv1.ReplicaSet{ObjectMeta: loadedMeta("web-abc"), Spec: appsv1.ReplicaSetSpec{Template: loadedTemplate()}},
		&batchv1.Job{ObjectMeta: loadedMeta("nightly"), Spec: batchv1.JobSpec{Template: loadedTemplate()}},
		&batchv1.CronJob{ObjectMeta: loadedMeta("nightly"), Spec: batchv1.CronJobSpec{
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: loadedTemplate()}},
		}},
		&corev1.Namespace{ObjectMeta: loadedMeta("shop")},
	}

	for _, obj := range objects {
		kind := reflect.TypeOf(obj).Elem().Name()
		if len(containersIn(obj)) == 0 && kind != "Namespace" {
			t.Fatalf("%s: the fixture carries no containers, so this row proves nothing", kind)
		}

		stored, err := dropUncollectedFields(obj)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		for _, c := range containersIn(stored) {
			if c.Env != nil || c.EnvFrom != nil || c.Command != nil || c.Args != nil {
				t.Errorf("%s: container %q reached the cache with env=%v envFrom=%v command=%v args=%v",
					kind, c.Name, c.Env, c.EnvFrom, c.Command, c.Args)
			}
			if c.Image == "" || c.Name == "" {
				t.Errorf("%s: container %q lost a field the payloads are built from", kind, c.Name)
			}
		}

		meta := reflect.ValueOf(stored).Elem().FieldByName("ObjectMeta").Interface().(metav1.ObjectMeta)
		if _, ok := meta.Annotations[lastAppliedAnnotation]; ok {
			t.Errorf("%s: the applied-configuration copy is still there, env and all", kind)
		}
		if meta.Annotations[CollectAnnotation] != "false" {
			t.Errorf("%s: the opt-out annotation was dropped with it; the filter reads that one", kind)
		}
		if meta.ManagedFields != nil {
			t.Errorf("%s: managed fields are cached and nothing reads them", kind)
		}
	}
}

// The transform is only worth as much as its wiring: a function that strips
// every field and is registered with nothing passes the test above.
func TestTheStrippedObjectIsWhatTheListersServe(t *testing.T) {
	p := pod("web", nil)
	p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "DATABASE_URL", Value: secretish}}
	p.Spec.Containers[0].Args = []string{"--token=t0ps3cret"}
	p.Annotations = map[string]string{lastAppliedAnnotation: secretish}

	watcher := NewPodWatcher(fake.NewClientset(p), func(PodInfo) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = watcher.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		cached, err := watcher.podLister.Pods("shop").Get("web")
		if err == nil {
			if got := cached.Spec.Containers[0]; got.Env != nil || got.Args != nil {
				t.Errorf("the cache holds env=%v args=%v for %q", got.Env, got.Args, got.Name)
			}
			if _, ok := cached.Annotations[lastAppliedAnnotation]; ok {
				t.Error("the cache holds the applied-configuration copy")
			}
			if cached.Spec.Containers[0].Image == "" {
				t.Error("the transform is wired but takes too much: the image is gone")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the pod never reached the cache: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
