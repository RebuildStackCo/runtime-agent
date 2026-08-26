//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	k8syaml "sigs.k8s.io/yaml"

	"github.com/RebuildStackCo/runtime-agent/internal/chartrender"
)

// The e2e installs the chart, and that is the point of the chart existing
// (ADR 0036).
//
// Before it, `deploy/*.yaml` were two artifacts at once: the reference an
// operator followed, and the input this suite parsed by hand. Nothing checked
// that they still described the same install, and the node's token audience,
// the controller's required audience and the pinned subject — three strings
// that must agree — were written out three times.
//
// What the tests still change is what a cluster forces them to: an image that
// exists only in kind, intervals shortened so a test finishes, and a shell
// sidecar so the payload of a distroless container can be read.

const chartPath = "../../" + chartrender.Dir

// installOptions is what an e2e install varies. Everything else comes from the
// chart, unmodified.
type installOptions struct {
	// profile is the chart's install profile; empty means metrics-only.
	profile string
	// values overlay the chart's defaults, in the shape of values.yaml.
	values map[string]any
	// spoolReader adds the shell sidecar that shares the controller's spool
	// volume, so a test can read what the distroless agent wrote.
	spoolReader bool
	// skipController and skipNode leave out one half of the release. A test
	// that wants a node without a controller reachable is testing failing
	// closed, which is a scenario rather than an install (ADR 0015).
	skipController bool
	skipNode       bool
	// narrowRole rewrites the ClusterRole before it is created — the only way
	// to start an agent that was never given a grant, since patching afterwards
	// races the pod's first LIST.
	narrowRole func(*rbacv1.ClusterRole)
	// mutateNode adjusts the DaemonSet for the cluster the test runs in.
	mutateNode func(*appsv1.DaemonSet)
}

// installChart renders the chart the way `helm install` would and creates every
// object it produces, minus what the options leave out.
func installChart(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, image string, opts installOptions) {
	t.Helper()

	profile := opts.profile
	if profile == "" {
		profile = "metrics-only"
	}
	repository, tag := splitImage(image)
	values := map[string]any{
		"profile": profile,
		"image": map[string]any{
			"repository": repository,
			"tag":        tag,
			// The agent image is loaded into kind, never pulled.
			"pullPolicy": "Never",
		},
	}
	for key, value := range opts.values {
		values[key] = value
	}

	docs, err := chartrender.Manifests(chartPath, chartrender.Options{
		ReleaseName: "runtime-agent",
		Namespace:   ns,
		Values:      values,
	})
	if err != nil {
		t.Fatalf("rendering the chart: %v", err)
	}

	for _, doc := range docs {
		var meta metav1.PartialObjectMetadata
		if err := k8syaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatalf("decoding rendered document: %v\n%s", err, doc)
		}
		component := meta.Labels["app.kubernetes.io/component"]
		if opts.skipController && component == "controller" {
			continue
		}
		if opts.skipNode && component == "node" {
			continue
		}

		switch meta.Kind {
		case "ServiceAccount":
			var sa corev1.ServiceAccount
			mustUnmarshal(t, []byte(doc), &sa)
			create(ctx, t, "ServiceAccount", func() error {
				_, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx, &sa, metav1.CreateOptions{})
				return err
			})
		case "ClusterRole":
			var cr rbacv1.ClusterRole
			mustUnmarshal(t, []byte(doc), &cr)
			if opts.narrowRole != nil {
				opts.narrowRole(&cr)
			}
			create(ctx, t, "ClusterRole", func() error {
				_, err := cs.RbacV1().ClusterRoles().Create(ctx, &cr, metav1.CreateOptions{})
				return err
			})
			cleanupClusterScoped(t, func(delCtx context.Context) {
				_ = cs.RbacV1().ClusterRoles().Delete(delCtx, cr.Name, metav1.DeleteOptions{})
			})
		case "ClusterRoleBinding":
			var crb rbacv1.ClusterRoleBinding
			mustUnmarshal(t, []byte(doc), &crb)
			create(ctx, t, "ClusterRoleBinding", func() error {
				_, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, &crb, metav1.CreateOptions{})
				return err
			})
			cleanupClusterScoped(t, func(delCtx context.Context) {
				_ = cs.RbacV1().ClusterRoleBindings().Delete(delCtx, crb.Name, metav1.DeleteOptions{})
			})
		case "ConfigMap":
			var cm corev1.ConfigMap
			mustUnmarshal(t, []byte(doc), &cm)
			create(ctx, t, "ConfigMap", func() error {
				_, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &cm, metav1.CreateOptions{})
				return err
			})
		case "Service":
			var svc corev1.Service
			mustUnmarshal(t, []byte(doc), &svc)
			create(ctx, t, "Service", func() error {
				_, err := cs.CoreV1().Services(ns).Create(ctx, &svc, metav1.CreateOptions{})
				return err
			})
		case "NetworkPolicy":
			// Created because a customer's install creates it, not because this
			// suite can prove it works: kind's default CNI does not implement
			// NetworkPolicy, so nothing here is enforcing it (ADR 0040). What
			// creating it does prove is that the object the chart renders is one
			// the API server accepts, and that the channel still works with it
			// present.
			var np networkingv1.NetworkPolicy
			mustUnmarshal(t, []byte(doc), &np)
			create(ctx, t, "NetworkPolicy", func() error {
				_, err := cs.NetworkingV1().NetworkPolicies(ns).Create(ctx, &np, metav1.CreateOptions{})
				return err
			})
		case "Deployment":
			var dep appsv1.Deployment
			mustUnmarshal(t, []byte(doc), &dep)
			if opts.spoolReader {
				addSpoolReader(&dep)
			}
			create(ctx, t, "Deployment", func() error {
				_, err := cs.AppsV1().Deployments(ns).Create(ctx, &dep, metav1.CreateOptions{})
				return err
			})
		case "DaemonSet":
			var ds appsv1.DaemonSet
			mustUnmarshal(t, []byte(doc), &ds)
			if opts.mutateNode != nil {
				opts.mutateNode(&ds)
			}
			create(ctx, t, "DaemonSet", func() error {
				_, err := cs.AppsV1().DaemonSets(ns).Create(ctx, &ds, metav1.CreateOptions{})
				return err
			})
		default:
			t.Fatalf("the chart rendered an unexpected kind %q; teach installChart about it", meta.Kind)
		}
	}
}

// addSpoolReader mounts the controller's spool into a shell container, since
// the agent image is distroless and cannot serve its own files.
func addSpoolReader(dep *appsv1.Deployment) {
	dep.Spec.Template.Spec.Containers = append(dep.Spec.Template.Spec.Containers, corev1.Container{
		Name:            "spool-reader",
		Image:           spoolReaderImage(),
		ImagePullPolicy: corev1.PullNever,
		Command:         []string{"sleep", "86400"},
		// The pod asserts runAsNonRoot, so kubelet refuses any container whose
		// image would run as root — busybox would. Running the sidecar as the
		// agent's own uid is also what lets it read the payloads: the spool's
		// files are 0600, owned by the controller (ADR 0037).
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptr.To(int64(65532)),
			RunAsNonRoot:             ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name: "spool", MountPath: "/var/spool/runtime-agent", ReadOnly: true,
		}},
	})
}

func cleanupClusterScoped(t *testing.T, del func(context.Context)) {
	t.Helper()
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		del(delCtx)
	})
}

// splitImage divides "repo:tag" at the last colon, leaving a digest-free
// repository that may itself contain a port.
func splitImage(image string) (repository, tag string) {
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, "latest"
}

// deployController installs the controller alone — the metrics-only profile,
// plus the sidecar that lets the test read its spool.
func deployController(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, image string) {
	t.Helper()
	installChart(ctx, t, cs, ns, image, installOptions{spoolReader: true})
}

// setNodeArgs replaces the node container's arguments. A test reaches for this
// only when the scenario is about an argument's absence — the endpoints the
// chart writes are derived from the release namespace and are what a customer
// gets, so overriding them would test something nobody installs.
func setNodeArgs(ds *appsv1.DaemonSet, args ...string) {
	ds.Spec.Template.Spec.Containers[0].Args = args
}

// controllerClusterRoleName is the name the chart gives the cluster-scoped role
// for a release in ns. Cluster-scoped objects carry the namespace so two
// installs — or two concurrent e2e runs — cannot collide.
func controllerClusterRoleName(ns string) string {
	return "runtime-agent-controller-" + ns
}
