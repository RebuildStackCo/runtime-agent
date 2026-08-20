//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	k8syaml "sigs.k8s.io/yaml"
)

const controllerManifestPath = "../../deploy/controller.yaml"

// spoolReaderImage is a shell-bearing image loaded into kind as a sidecar in
// the controller pod: the agent image is distroless (no shell), so the sidecar
// is how the test reads the spool file the controller writes. `make
// inventory-e2e` loads it.
func spoolReaderImage() string {
	if img := os.Getenv("E2E_SPOOL_READER_IMAGE"); img != "" {
		return img
	}
	return "busybox:1.37"
}

const spoolPath = "/var/spool/runtime-agent/go-inventory.json"

// TestGoInventoryEndToEnd exercises the whole node→controller inventory path in
// a real cluster (ADR 0010): a known Go workload runs, the node DaemonSet scans
// it and ships the fact to the controller over HTTP with a projected token, the
// controller validates the token against the cluster JWKS, joins the fact
// against its workload inventory, and writes a go_inventory payload to its
// spool. The test asserts on the spool contents — the payload the backend would
// receive — not on logs (docs/development.md).
//
// Gated on E2E_AGENT_IMAGE / E2E_SAMPLE_IMAGE (kind-loaded); use
// `make inventory-e2e`.
func TestGoInventoryEndToEnd(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	sampleImage := os.Getenv("E2E_SAMPLE_IMAGE")
	if agentImage == "" || sampleImage == "" {
		t.Skip("E2E_AGENT_IMAGE / E2E_SAMPLE_IMAGE not set; run `make inventory-e2e`")
	}
	config := clusterConfig(t)
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-inv-e2e-%d", os.Getpid())
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		_ = clientset.CoreV1().Namespaces().Delete(cleanupCtx, ns, metav1.DeleteOptions{})
	})

	// The known Go workload as a Deployment, so the controller resolves it to a
	// workload (Pod → ReplicaSet → Deployment) — exercising the owner chain.
	deploySampleWorkload(ctx, t, clientset, ns, sampleImage)

	// The controller: read RBAC, JWKS discovery grant, node-intake receiver, and
	// a spool with a shell sidecar to read it.
	deployController(ctx, t, clientset, ns, agentImage, renderControllerConfig(ns))
	controllerPod := waitDeploymentPod(ctx, t, clientset, ns, "controller")
	t.Logf("controller pod: %s", controllerPod)

	// The node DaemonSet, pointed at the controller Service in this namespace.
	base := fmt.Sprintf("http://runtime-agent-controller.%s.svc.cluster.local:8080", ns)
	deployNodeDaemonSet(ctx, t, clientset, ns, agentImage, base+"/v1/node-inventory", base+"/v1/node-scope")
	nodePod := waitDaemonSetPod(ctx, t, clientset, ns)
	t.Logf("node pod: %s", nodePod)

	// Poll the controller's spool until the go_inventory payload carries the
	// sample workload. Delivery is periodic (the node's scan interval and the
	// controller's flush cadence), so allow generous time.
	deadline := time.Now().Add(6 * time.Minute)
	for {
		if rec, ok := findSampleRecord(ctx, t, config, clientset, ns, controllerPod); ok {
			if !strings.HasPrefix(rec.GoVersion, "go1.") {
				t.Errorf("go_version = %q, want a go1.x version", rec.GoVersion)
			}
			if rec.ImageDigest == "" {
				t.Errorf("image_digest empty; want the sample image content digest")
			}
			if !strings.HasPrefix(rec.ImageDigest, "sha256:") {
				t.Errorf("image_digest = %q, want a sha256: digest", rec.ImageDigest)
			}
			if rec.PGO {
				t.Errorf("pgo = true; the sample is built without PGO")
			}
			if rec.Container != "goworkload" {
				t.Errorf("container = %q, want goworkload", rec.Container)
			}
			if rec.WorkloadKind != "Deployment" || rec.WorkloadName != "goworkload" {
				t.Errorf("workload = %s/%s, want Deployment/goworkload", rec.WorkloadKind, rec.WorkloadName)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("go_inventory payload never carried the sample workload (%s) before timeout", sampleModulePath)
		}
		time.Sleep(5 * time.Second)
	}
}

// goInventoryRecord mirrors one record of the go_inventory payload.
type goInventoryRecord struct {
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workload_kind"`
	WorkloadName string `json:"workload_name"`
	Container    string `json:"container"`
	GoVersion    string `json:"go_version"`
	ModulePath   string `json:"module_path"`
	ImageDigest  string `json:"image_digest"`
	PGO          bool   `json:"pgo"`
}

// findSampleRecord reads the controller's spool file via the sidecar and returns
// the record for the sample module, if the payload exists yet.
func findSampleRecord(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod string) (goInventoryRecord, bool) {
	t.Helper()
	raw, ok := readSpoolFile(ctx, t, config, cs, ns, pod)
	if !ok {
		return goInventoryRecord{}, false
	}
	var payload struct {
		Kind    string              `json:"kind"`
		Records []goInventoryRecord `json:"records"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Logf("spool file not valid JSON yet (will retry): %v", err)
		return goInventoryRecord{}, false
	}
	if payload.Kind != "go_inventory" {
		t.Errorf("payload kind = %q, want go_inventory", payload.Kind)
	}
	for _, r := range payload.Records {
		if r.ModulePath == sampleModulePath {
			return r, true
		}
	}
	return goInventoryRecord{}, false
}

// readSpoolFile cats the go_inventory file from the controller pod's spool via
// the shell sidecar. A non-zero exit (file not written yet) returns ok=false so
// the caller retries.
func readSpoolFile(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod string) (string, bool) {
	t.Helper()
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "spool-reader",
			Command:   []string{"cat", spoolPath},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		t.Fatalf("building exec: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		// cat exits non-zero until the controller writes the file — expected
		// during the poll, not a test failure.
		return "", false
	}
	return stdout.String(), true
}

// deploySampleWorkload creates the known Go workload as a Deployment and waits
// for its pod to run.
func deploySampleWorkload(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, image string) {
	t.Helper()
	replicas := int32(1)
	labels := map[string]string{"app": "goworkload"}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "goworkload"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "goworkload",
						Image:           image,
						ImagePullPolicy: corev1.PullNever,
					}},
				},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating sample deployment: %v", err)
	}
	waitDeploymentPod(ctx, t, cs, ns, "goworkload")
}

// deployController decodes deploy/controller.yaml and applies it into ns. It
// retargets the namespace, uniquely renames the cluster-scoped RBAC objects (so
// concurrent runs and cleanup are safe), points them at this run's
// ServiceAccount, overrides the image, installs configYAML as the ConfigMap for
// this namespace, and adds the shell sidecar that shares the spool volume so the
// test can read the payload the controller writes.
func deployController(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, image, configYAML string) {
	t.Helper()
	data, err := os.ReadFile(controllerManifestPath)
	if err != nil {
		t.Fatalf("reading manifest %s: %v", controllerManifestPath, err)
	}
	clusterRoleName := "runtime-agent-controller-" + ns
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading manifest doc: %v", err)
		}
		if strings.TrimSpace(string(doc)) == "" {
			continue
		}
		var tm metav1.TypeMeta
		if err := k8syaml.Unmarshal(doc, &tm); err != nil {
			t.Fatalf("decoding doc TypeMeta: %v", err)
		}
		switch tm.Kind {
		case "", "Namespace":
			// The test manages its own namespace.
		case "ServiceAccount":
			var sa corev1.ServiceAccount
			mustUnmarshal(t, doc, &sa)
			sa.Namespace = ns
			create(ctx, t, "ServiceAccount", func() error {
				_, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx, &sa, metav1.CreateOptions{})
				return err
			})
		case "ClusterRole":
			var cr rbacv1.ClusterRole
			mustUnmarshal(t, doc, &cr)
			cr.Name = clusterRoleName
			create(ctx, t, "ClusterRole", func() error {
				_, err := cs.RbacV1().ClusterRoles().Create(ctx, &cr, metav1.CreateOptions{})
				return err
			})
			t.Cleanup(func() {
				delCtx, c := context.WithTimeout(context.Background(), time.Minute)
				defer c()
				_ = cs.RbacV1().ClusterRoles().Delete(delCtx, clusterRoleName, metav1.DeleteOptions{})
			})
		case "ClusterRoleBinding":
			var crb rbacv1.ClusterRoleBinding
			mustUnmarshal(t, doc, &crb)
			crb.Name = clusterRoleName
			crb.RoleRef.Name = clusterRoleName
			for i := range crb.Subjects {
				crb.Subjects[i].Namespace = ns
			}
			create(ctx, t, "ClusterRoleBinding", func() error {
				_, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, &crb, metav1.CreateOptions{})
				return err
			})
			t.Cleanup(func() {
				delCtx, c := context.WithTimeout(context.Background(), time.Minute)
				defer c()
				_ = cs.RbacV1().ClusterRoleBindings().Delete(delCtx, clusterRoleName, metav1.DeleteOptions{})
			})
		case "ConfigMap":
			var cm corev1.ConfigMap
			mustUnmarshal(t, doc, &cm)
			cm.Namespace = ns
			cm.Data["config.yaml"] = configYAML
			create(ctx, t, "ConfigMap", func() error {
				_, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &cm, metav1.CreateOptions{})
				return err
			})
		case "Service":
			var svc corev1.Service
			mustUnmarshal(t, doc, &svc)
			svc.Namespace = ns
			create(ctx, t, "Service", func() error {
				_, err := cs.CoreV1().Services(ns).Create(ctx, &svc, metav1.CreateOptions{})
				return err
			})
		case "Deployment":
			var dep appsv1.Deployment
			mustUnmarshal(t, doc, &dep)
			dep.Namespace = ns
			c := &dep.Spec.Template.Spec.Containers[0]
			c.Image = image
			c.ImagePullPolicy = corev1.PullNever
			// Add the shell sidecar sharing the spool volume (read-only), so the
			// test can cat the payload the distroless controller cannot serve.
			dep.Spec.Template.Spec.Containers = append(dep.Spec.Template.Spec.Containers, corev1.Container{
				Name:            "spool-reader",
				Image:           spoolReaderImage(),
				ImagePullPolicy: corev1.PullNever,
				Command:         []string{"sleep", "86400"},
				VolumeMounts: []corev1.VolumeMount{{
					Name: "spool", MountPath: "/var/spool/runtime-agent", ReadOnly: true,
				}},
			})
			create(ctx, t, "Deployment", func() error {
				_, err := cs.AppsV1().Deployments(ns).Create(ctx, &dep, metav1.CreateOptions{})
				return err
			})
		default:
			t.Fatalf("unexpected manifest kind %q", tm.Kind)
		}
	}
}

func renderControllerConfig(ns string) string {
	return fmt.Sprintf(`spool:
  dir: /var/spool/runtime-agent
nodeIntake:
  enabled: true
  listenAddress: ":8080"
  audience: rebuildstack-controller
  expectedSubject: "system:serviceaccount:%s:runtime-agent-node"
`, ns)
}

func mustUnmarshal(t *testing.T, doc []byte, into any) {
	t.Helper()
	if err := k8syaml.Unmarshal(doc, into); err != nil {
		t.Fatalf("decoding manifest doc: %v", err)
	}
}

func create(ctx context.Context, t *testing.T, kind string, fn func() error) {
	t.Helper()
	if err := fn(); err != nil {
		t.Fatalf("creating %s: %v", kind, err)
	}
}

// waitDeploymentPod waits for a Running pod of the named component (matched by
// the app label the Deployment sets) and returns its name.
func waitDeploymentPod(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name string) string {
	t.Helper()
	selectors := []string{
		"app.kubernetes.io/component=" + name,
		"app=" + name,
	}
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		for _, sel := range selectors {
			pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
			if err != nil {
				continue
			}
			for _, p := range pods.Items {
				if p.Status.Phase == corev1.PodRunning {
					return p.Name
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no Running pod for %q in %s before timeout", name, ns)
	return ""
}
