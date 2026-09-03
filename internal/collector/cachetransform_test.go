package collector

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
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
		Env: []corev1.EnvVar{
			{Name: "DATABASE_URL", Value: secretish},
			{Name: "GOMAXPROCS", Value: "2"},
		},
		EnvFrom: []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}},
		}},
		// A probe's handler is the second place a command is written and the
		// only place an operator writes an HTTP header (ADR 0048 §1).
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
				Command: []string{"/bin/health", "--token=t0ps3cret"},
			}},
			PeriodSeconds: 10, FailureThreshold: 3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/internal/ready?key=t0ps3cret",
				Host: "db.internal",
				HTTPHeaders: []corev1.HTTPHeader{
					{Name: "Authorization", Value: "Bearer " + secretish},
				},
			}},
			PeriodSeconds: 5, FailureThreshold: 6,
		},
		StartupProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Host: "db.internal"}},
			PeriodSeconds: 1, FailureThreshold: 30,
		},
		Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{Command: []string{"/bin/drain", "--token=t0ps3cret"}},
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

func envNames(env []corev1.EnvVar) []string {
	var names []string
	for _, v := range env {
		names = append(names, v.Name)
	}
	return names
}

// The allow-list is a list of names, and a name is not a promise about where
// the value came from: a variable called GOGC that reads a Secret is a Secret
// read (ADR 0047).
func TestARuntimeKnobIsKeptOnlyWhenItsValueIsNotASecret(t *testing.T) {
	secretRef := &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "url",
	}}
	configRef := &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"}, Key: "gogc",
	}}
	limitRef := &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}

	c := corev1.Container{Name: "app", Env: []corev1.EnvVar{
		{Name: "GOGC", ValueFrom: secretRef},
		{Name: "GOTRACEBACK", ValueFrom: configRef},
		{Name: "GOMAXPROCS", ValueFrom: limitRef},
		{Name: "GOMEMLIMIT", Value: "900MiB"},
		{Name: "DATABASE_URL", Value: secretish},
		{Name: "GOPATH", Value: "/go"},
	}}
	dropContainerFields(&c)

	if got, want := envNames(c.Env), []string{"GOMAXPROCS", "GOMEMLIMIT"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	if got := runtimeEnvOf(&c); !reflect.DeepEqual(got, map[string]string{
		"GOMAXPROCS": "resource:limits.cpu",
		"GOMEMLIMIT": "900MiB",
	}) {
		t.Errorf("runtime env = %v; a knob read from the container's own limits must name the field", got)
	}
}

// "Not collected" beats "collected and dropped": the cache is the source, so
// the fields have to go before the object is stored, not before it is
// serialized (CLAUDE.md invariant 4, ADR 0046).
func TestNoWatchedKindEntersTheCacheCarryingEnvArgsOrCommand(t *testing.T) { //nolint:gocognit // one loop over every watched kind
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
			if c.EnvFrom != nil || c.Command != nil || c.Args != nil {
				t.Errorf("%s: container %q reached the cache with envFrom=%v command=%v args=%v",
					kind, c.Name, c.EnvFrom, c.Command, c.Args)
			}
			if names := envNames(c.Env); !reflect.DeepEqual(names, []string{"GOMAXPROCS"}) {
				t.Errorf("%s: container %q kept env %v, want only the allow-listed knob", kind, c.Name, names)
			}
			if c.Image == "" || c.Name == "" {
				t.Errorf("%s: container %q lost a field the payloads are built from", kind, c.Name)
			}
			checkProbesReduced(t, kind, c)
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

	watcher := NewPodWatcher(fake.NewClientset(p), func(model.PodInfo) {})
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

// checkProbesReduced is the other half of the same promise: a probe's handler
// holds a command and an operator's headers, and the schedule beside it is what
// the finding reads. So the handler must arrive emptied and the schedule intact
// (ADR 0048 §1).
func checkProbesReduced(t *testing.T, kind string, c corev1.Container) {
	t.Helper()
	if c.Lifecycle != nil {
		t.Errorf("%s: container %q reached the cache with lifecycle hooks; they hold commands nothing reads", kind, c.Name)
	}
	for name, p := range map[string]*corev1.Probe{
		"liveness": c.LivenessProbe, "readiness": c.ReadinessProbe, "startup": c.StartupProbe,
	} {
		if p == nil {
			t.Errorf("%s: container %q lost its %s probe; the schedule is what the finding reads", kind, c.Name, name)
			continue
		}
		if p.PeriodSeconds == 0 || p.FailureThreshold == 0 {
			t.Errorf("%s: container %q %s probe lost its schedule (%+v)", kind, c.Name, name, p)
		}
		if h := p.Exec; h != nil && h.Command != nil {
			t.Errorf("%s: container %q %s probe reached the cache with a command %v", kind, c.Name, name, h.Command)
		}
		if h := p.HTTPGet; h != nil && (h.Path != "" || h.Host != "" || h.HTTPHeaders != nil) {
			t.Errorf("%s: container %q %s probe reached the cache with path=%q host=%q headers=%v",
				kind, c.Name, name, h.Path, h.Host, h.HTTPHeaders)
		}
		if h := p.TCPSocket; h != nil && h.Host != "" {
			t.Errorf("%s: container %q %s probe reached the cache with host %q", kind, c.Name, name, h.Host)
		}
		if h := p.GRPC; h != nil && h.Service != nil {
			t.Errorf("%s: container %q %s probe reached the cache with service %q", kind, c.Name, name, *h.Service)
		}
	}
}

// The addresses in an EndpointSlice are pod IPs and its targetRef is a pod's
// name and UID. The zone counts need none of them, so the promise is stronger
// than "not shipped": they are never held (ADR 0051 §1).
func TestNoEndpointIdentityEntersTheCache(t *testing.T) {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "web-abc",
			Labels: map[string]string{discoveryv1.LabelServiceName: "web"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Port: ptr.To(int32(8080))}},
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:          []string{"10.4.1.7"},
			Hostname:           ptr.To("web-1"),
			NodeName:           ptr.To("node-1"),
			Zone:               ptr.To("eu-west-1a"),
			TargetRef:          &corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "web-1", UID: "uid-web-1"},
			DeprecatedTopology: map[string]string{"kubernetes.io/hostname": "node-1"},
			Conditions:         discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		}},
	}

	out, err := dropUncollectedFields(slice)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(*discoveryv1.EndpointSlice)
	e := got.Endpoints[0]
	if e.Addresses != nil {
		t.Errorf("endpoint reached the cache with addresses %v", e.Addresses)
	}
	if e.TargetRef != nil {
		t.Errorf("endpoint reached the cache with a targetRef %+v", e.TargetRef)
	}
	if e.NodeName != nil || e.Hostname != nil {
		t.Errorf("endpoint reached the cache with node %v / hostname %v", e.NodeName, e.Hostname)
	}
	if e.DeprecatedTopology != nil {
		t.Errorf("endpoint reached the cache with deprecated topology %v", e.DeprecatedTopology)
	}
	if got.Ports != nil {
		t.Errorf("slice reached the cache with ports %+v", got.Ports)
	}

	// What the counting reads must survive, or the transform has taken the
	// payload with the identity.
	if ptr.Deref(e.Zone, "") != "eu-west-1a" {
		t.Errorf("zone = %v, want eu-west-1a", e.Zone)
	}
	if !ptr.Deref(e.Conditions.Ready, false) {
		t.Error("the ready condition did not survive the transform")
	}
	if got.AddressType != discoveryv1.AddressTypeIPv4 {
		t.Errorf("address type = %v, want IPv4", got.AddressType)
	}
}
