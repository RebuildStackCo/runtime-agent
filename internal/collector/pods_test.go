package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func controllerRef(kind, name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "v1", Kind: kind, Name: name, UID: types.UID("uid-" + name),
		Controller: ptr.To(true),
	}
}

func pod(name string, owner *metav1.OwnerReference) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, UID: types.UID("uid-" + name)},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			InitContainers: []corev1.Container{
				{Name: "init-db", Image: "example.com/migrate:v3", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				}},
			},
			Containers: []corev1.Container{
				{Name: "app", Image: "example.com/app:1.2.3", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				}, Ports: []corev1.ContainerPort{
					{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
					{ContainerPort: 9090, Protocol: corev1.ProtocolTCP}, // metrics, unnamed
				}},
				{Name: "sidecar", Image: "example.com/proxy:latest"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable},
	}
	if owner != nil {
		p.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return p
}

// collectPods runs a PodWatcher over the given objects and returns everything
// reported until at least want pods are seen or the timeout hits.
func collectPods(t *testing.T, want int, objects ...runtime.Object) map[string]PodInfo {
	t.Helper()
	clientset := fake.NewClientset(objects...)

	var mu sync.Mutex
	seen := make(map[string]PodInfo)
	got := make(chan struct{}, 64)
	watcher := NewPodWatcher(clientset, func(p PodInfo) {
		mu.Lock()
		seen[p.Namespace+"/"+p.Name] = p
		mu.Unlock()
		got <- struct{}{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for n := 0; n < want; {
		select {
		case <-got:
			n++
		case <-deadline:
			t.Fatalf("saw %d pods before timeout, want %d", n, want)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return seen
}

func TestResolvesWorkloadChains(t *testing.T) {
	rsOwnedByDeployment := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "checkout-7d9f", UID: "uid-checkout-7d9f",
		OwnerReferences: []metav1.OwnerReference{controllerRef("Deployment", "checkout")},
	}}
	rsOwnedByRollout := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "payments-5c4b", UID: "uid-payments-5c4b",
		OwnerReferences: []metav1.OwnerReference{controllerRef("Rollout", "payments")},
	}}
	jobOwnedByCronJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "report-29000", UID: "uid-report-29000",
		OwnerReferences: []metav1.OwnerReference{controllerRef("CronJob", "report")},
	}}
	bareJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "one-off", UID: "uid-one-off",
	}}

	seen := collectPods(t, 7,
		rsOwnedByDeployment, rsOwnedByRollout, jobOwnedByCronJob, bareJob,
		pod("checkout-7d9f-x1", ptr.To(controllerRef("ReplicaSet", "checkout-7d9f"))),
		pod("payments-5c4b-x1", ptr.To(controllerRef("ReplicaSet", "payments-5c4b"))),
		pod("report-29000-x1", ptr.To(controllerRef("Job", "report-29000"))),
		pod("one-off-x1", ptr.To(controllerRef("Job", "one-off"))),
		pod("db-0", ptr.To(controllerRef("StatefulSet", "db"))),
		pod("node-exporter-abc", ptr.To(controllerRef("DaemonSet", "node-exporter"))),
		pod("debug-shell", nil),
	)

	want := map[string]WorkloadRef{
		"shop/checkout-7d9f-x1":  {Kind: "Deployment", Name: "checkout"},
		"shop/payments-5c4b-x1":  {Kind: "Rollout", Name: "payments"},
		"shop/report-29000-x1":   {Kind: "CronJob", Name: "report"},
		"shop/one-off-x1":        {Kind: "Job", Name: "one-off"},
		"shop/db-0":              {Kind: "StatefulSet", Name: "db"},
		"shop/node-exporter-abc": {Kind: "DaemonSet", Name: "node-exporter"},
		"shop/debug-shell":       {Kind: "none"},
	}
	for key, workload := range want {
		info, ok := seen[key]
		if !ok {
			t.Errorf("pod %s was not reported", key)
			continue
		}
		if info.Workload != workload {
			t.Errorf("pod %s: workload = %+v, want %+v", key, info.Workload, workload)
		}
	}
}

func TestCollectsContainersAndImages(t *testing.T) {
	seen := collectPods(t, 1, pod("checkout-1", nil))

	info := seen["shop/checkout-1"]
	want := []Container{
		{Name: "init-db", Image: "example.com/migrate:v3", Init: true,
			Resources: Resources{CPURequestMilli: ptr.To(int64(100))}},
		{Name: "app", Image: "example.com/app:1.2.3",
			Resources: Resources{
				CPURequestMilli:    ptr.To(int64(500)),
				CPULimitMilli:      ptr.To(int64(2000)),
				MemoryRequestBytes: ptr.To(int64(256 << 20)),
				MemoryLimitBytes:   ptr.To(int64(1 << 30)),
			},
			Ports: []ContainerPort{
				{Name: "http", Port: 8080, Protocol: "TCP"},
				{Port: 9090, Protocol: "TCP"},
			}},
		{Name: "sidecar", Image: "example.com/proxy:latest"},
	}
	if len(info.Containers) != len(want) {
		t.Fatalf("containers = %+v, want %+v", info.Containers, want)
	}
	for i := range want {
		got := info.Containers[i]
		if got.Name != want[i].Name || got.Image != want[i].Image || got.Init != want[i].Init {
			t.Errorf("container %d = %+v, want %+v", i, got, want[i])
		}
		// No container statuses in the fixture: digests appear only once a
		// container starts (see TestReportsImageDigestOnUpdate).
		if got.ImageDigest != "" {
			t.Errorf("container %s: image digest = %q, want empty before start", got.Name, got.ImageDigest)
		}
		if !reflect.DeepEqual(got.Ports, want[i].Ports) {
			t.Errorf("container %s: ports = %+v, want %+v", got.Name, got.Ports, want[i].Ports)
		}
		assertResources(t, got.Name, got.Resources, want[i].Resources)
	}
	if info.Node != "node-1" || info.Phase != string(corev1.PodRunning) {
		t.Errorf("node/phase = %q/%q, want node-1/Running", info.Node, info.Phase)
	}
	if info.QOSClass != string(corev1.PodQOSBurstable) {
		t.Errorf("qos = %q, want Burstable", info.QOSClass)
	}
}

// assertResources compares two Resources field by field; a nil on either
// side must be nil on the other (absent means "not set", not zero).
func assertResources(t *testing.T, container string, got, want Resources) {
	t.Helper()
	check := func(field string, g, w *int64) {
		switch {
		case (g == nil) != (w == nil):
			t.Errorf("%s: %s = %v, want %v", container, field, format(g), format(w))
		case g != nil && *g != *w:
			t.Errorf("%s: %s = %d, want %d", container, field, *g, *w)
		}
	}
	check("cpu_request_milli", got.CPURequestMilli, want.CPURequestMilli)
	check("cpu_limit_milli", got.CPULimitMilli, want.CPULimitMilli)
	check("memory_request_bytes", got.MemoryRequestBytes, want.MemoryRequestBytes)
	check("memory_limit_bytes", got.MemoryLimitBytes, want.MemoryLimitBytes)
}

func format(v *int64) string {
	if v == nil {
		return "unset"
	}
	return fmt.Sprintf("%d", *v)
}

func TestReportsPodsCreatedAfterStart(t *testing.T) {
	clientset := fake.NewClientset(pod("existing", nil))

	events := make(chan PodInfo, 16)
	watcher := NewPodWatcher(clientset, func(p PodInfo) { events <- p })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	waitFor := func(name string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case p := <-events:
				if p.Name == name {
					return
				}
			case <-deadline:
				t.Fatalf("pod %s was not reported before timeout", name)
			}
		}
	}

	waitFor("existing")

	newPod := pod("created-later", ptr.To(controllerRef("StatefulSet", "db")))
	if _, err := clientset.CoreV1().Pods("shop").Create(ctx, newPod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating pod: %v", err)
	}
	waitFor("created-later")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
}

func TestParseImageDigest(t *testing.T) {
	cases := []struct {
		name    string
		imageID string
		want    string
	}{
		{"registry with digest", "example.com/app@sha256:abc123", "sha256:abc123"},
		{"docker-pullable prefix", "docker-pullable://registry.k8s.io/pause@sha256:def456", "sha256:def456"},
		{"registry with port and path", "registry.internal:5000/team/app@sha256:0011ff", "sha256:0011ff"},
		{"bare digest", "sha256:cafebabe", "sha256:cafebabe"},
		{"empty", "", ""},
		{"tag only, no digest yet", "example.com/app:1.2.3", ""},
		{"local reference without digest", "docker.io/library/redis:7", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseImageDigest(c.imageID); got != c.want {
				t.Errorf("parseImageDigest(%q) = %q, want %q", c.imageID, got, c.want)
			}
		})
	}
}

func TestNormalizeContainerID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"containerd://ABCDEF0123", "abcdef0123"},
		{"cri-o://abcDEF", "abcdef"},
		{"docker://DeadBeef", "deadbeef"},
		{"deadbeef", "deadbeef"}, // already bare (as the node cgroup yields)
		{"", ""},
		{"  containerd://Ff  ", "ff"},
	}
	for _, c := range cases {
		if got := normalizeContainerID(c.in); got != c.want {
			t.Errorf("normalizeContainerID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLookupContainerJoinsNodeFact verifies the join the node→controller channel
// depends on (ADR 0010): a pod UID + container ID resolve to the workload,
// container name, and image digest the controller collected. It also checks the
// negative paths — unknown pod, unknown container — that the join counts as
// unjoined rather than guessing.
func TestLookupContainerJoinsNodeFact(t *testing.T) {
	owner := controllerRef("ReplicaSet", "web-7d9f")
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web-7d9f", UID: "uid-web-7d9f",
		OwnerReferences: []metav1.OwnerReference{controllerRef("Deployment", "web")},
	}}
	p := pod("web-7d9f-abcde", &owner)
	// The runtime assigns container IDs and the kubelet reports digests once the
	// containers start — the shape the node scanner then observes on the node.
	p.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "app", ContainerID: "containerd://AAA111", ImageID: "example.com/app@sha256:appdigest"},
		{Name: "sidecar", ContainerID: "containerd://bbb222", ImageID: "example.com/proxy@sha256:proxydigest"},
	}

	clientset := fake.NewClientset(rs, p)
	watcher := NewPodWatcher(clientset, func(PodInfo) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	// Wait until the container index has the started container (populated on the
	// status update path).
	var ns string
	var workload WorkloadRef
	var container, digest string
	deadline := time.After(5 * time.Second)
	for {
		var ok bool
		ns, workload, container, digest, ok = watcher.LookupContainer("uid-web-7d9f-abcde", "aaa111")
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("container never became resolvable")
		case <-time.After(20 * time.Millisecond):
		}
	}

	if ns != "shop" || workload.Kind != "Deployment" || workload.Name != "web" {
		t.Errorf("resolved workload = %s/%s/%s, want shop/Deployment/web", ns, workload.Kind, workload.Name)
	}
	if container != "app" {
		t.Errorf("container = %q, want app", container)
	}
	if digest != "sha256:appdigest" {
		t.Errorf("image digest = %q, want sha256:appdigest", digest)
	}

	// Unknown container ID within a known pod: not joinable.
	if _, _, _, _, ok := watcher.LookupContainer("uid-web-7d9f-abcde", "ffffff"); ok {
		t.Error("unknown container ID resolved; want not-ok")
	}
	// Unknown pod: not joinable.
	if _, _, _, _, ok := watcher.LookupContainer("uid-nope", "aaa111"); ok {
		t.Error("unknown pod UID resolved; want not-ok")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
}

// TestReportsImageDigestOnUpdate covers the load-bearing timing fact: imageID
// is empty on the initial add and appears only on the status update that
// follows container start, so the pod must be re-reported for the digest to
// reach a consumer.
func TestReportsImageDigestOnUpdate(t *testing.T) {
	// The pod starts with no container statuses, exactly as it is before its
	// containers run.
	p := pod("checkout-1", nil)
	clientset := fake.NewClientset(p)

	events := make(chan PodInfo, 16)
	watcher := NewPodWatcher(clientset, func(info PodInfo) { events <- info })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()

	digestOf := func(info PodInfo, container string) (string, bool) {
		for _, c := range info.Containers {
			if c.Name == container {
				return c.ImageDigest, true
			}
		}
		return "", false
	}

	// First report: the container has not started, so no digest is known.
	deadline := time.After(5 * time.Second)
	select {
	case info := <-events:
		if d, ok := digestOf(info, "app"); !ok || d != "" {
			t.Fatalf("initial report: app digest = %q (present=%v), want empty", d, ok)
		}
	case <-deadline:
		t.Fatal("no initial pod report before timeout")
	}

	// The kubelet learns the image digests once the containers start and
	// writes them into status — delivered as an update event.
	p.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "app", ImageID: "example.com/app@sha256:appdigest"},
		{Name: "sidecar", ImageID: "docker-pullable://example.com/proxy@sha256:proxydigest"},
	}
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: "init-db", ImageID: "example.com/migrate@sha256:initdigest"},
	}
	if _, err := clientset.CoreV1().Pods("shop").Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating pod status: %v", err)
	}

	// A later report must carry the digests parsed from imageID.
	deadline = time.After(5 * time.Second)
	for {
		select {
		case info := <-events:
			d, ok := digestOf(info, "app")
			if !ok || d == "" {
				continue // an earlier, pre-update report still draining
			}
			if d != "sha256:appdigest" {
				t.Fatalf("app digest = %q, want sha256:appdigest", d)
			}
			if got, _ := digestOf(info, "sidecar"); got != "sha256:proxydigest" {
				t.Errorf("sidecar digest = %q, want sha256:proxydigest", got)
			}
			if got, _ := digestOf(info, "init-db"); got != "sha256:initdigest" {
				t.Errorf("init-db digest = %q, want sha256:initdigest", got)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("watcher returned error: %v", err)
			}
			return
		case <-deadline:
			t.Fatal("no pod report carrying the image digest before timeout")
		}
	}
}

func TestContainersOnNode(t *testing.T) {
	owner := controllerRef("ReplicaSet", "web-7d9f")
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web-7d9f", UID: "uid-web-7d9f",
		OwnerReferences: []metav1.OwnerReference{controllerRef("Deployment", "web")},
	}}
	p := pod("web-7d9f-abcde", &owner)
	p.Spec.NodeName = "node-1"
	p.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "app", ContainerID: "containerd://AAA111", ImageID: "example.com/app@sha256:d"},
	}

	clientset := fake.NewClientset(rs, p)
	watcher := NewPodWatcher(clientset, func(PodInfo) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = watcher.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		cs := watcher.ContainersOnNode("node-1")
		if len(cs) > 0 {
			if cs[0].Namespace != "shop" || cs[0].Workload.Name != "web" || cs[0].ContainerID != "aaa111" {
				t.Errorf("container on node = %+v, want shop/Deployment web/aaa111", cs[0])
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("container never appeared on node-1")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if cs := watcher.ContainersOnNode("other-node"); len(cs) != 0 {
		t.Errorf("other-node should have no containers, got %+v", cs)
	}
}
