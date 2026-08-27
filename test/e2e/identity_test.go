//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/ptr"
)

// The node→controller channel authenticates a *node*, not just the node role
// (ADR 0040), and the property is about what the kubelet puts in a token, which
// no unit test can establish. Requests come from inside the cluster through a
// shell sidecar on the DaemonSet, using the node's own mounted token.
//
// What it cannot show: kind's default CNI does not implement NetworkPolicy, so
// the policy the chart ships is created here and enforced by nothing.
// Reachability is out of scope; identity is the whole of it.

const probeContainer = "channel-probe"

// TestANodeCannotSpeakForAnotherNode is the cross-node case end to end: a
// genuine kubelet-projected token is used to ask each endpoint about a node that
// is not the caller's, and every one must refuse. Before ADR 0040 they all
// returned data, because the node name came from the request body.
//
// Gated on E2E_AGENT_IMAGE (kind-loaded); use `make identity-e2e`.
func TestANodeCannotSpeakForAnotherNode(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	if agentImage == "" {
		t.Skip("E2E_AGENT_IMAGE not set; run `make identity-e2e`")
	}
	config := clusterConfig(t)
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	ns := uniqueNamespace(ctx, t, clientset, "runtime-agent-identity-e2e")
	// The `ebpf` profile, because it is the only one that mounts all four
	// endpoints — the targets route exists only when the controller is
	// answering profiling queries. Installed with `inventory` first, the
	// targets case passed on a 404: the route was absent, the refusal was the
	// mux's, and the test would have gone on reporting a check that had never
	// run. The profiler itself may refuse on this kernel; that is fine, the
	// node keeps running as the scanner and the endpoints are what is under
	// test here.
	installChart(ctx, t, clientset, ns, agentImage, installOptions{
		profile: "ebpf",
		values: map[string]any{
			"profiling": map[string]any{"allowedModulePrefixes": []any{"example.com/"}},
		},
		mutateNode: addChannelProbe,
	})

	// The controller must be answering before a refusal means anything: a
	// connection refused would satisfy "not 200" without proving a thing.
	waitDeploymentPod(ctx, t, clientset, ns, "controller")
	probePod := waitDaemonSetPod(ctx, t, clientset, ns)
	ownNode := podNode(ctx, t, clientset, ns, probePod)

	endpoint := fmt.Sprintf("http://runtime-agent-controller.%s.svc:8080", ns)
	const foreign = "definitely-not-this-node"

	// First the positive control. Without it, a refusal below could mean the
	// check works or could mean nothing works.
	t.Run("its own node is answered", func(t *testing.T) {
		status := probeReceiver(ctx, t, config, clientset, ns, probePod,
			endpoint+"/v1/node-scope", "", fmt.Sprintf(`{"node":%q}`, ownNode))
		if status != 200 {
			t.Fatalf("scope query for the caller's own node = %d, want 200", status)
		}
	})

	cases := []struct {
		name string
		path string
		body string
		what string
	}{
		{
			name: "scope",
			path: "/v1/node-scope",
			body: fmt.Sprintf(`{"node":%q}`, foreign),
			what: "read which pods the controller admitted on another node",
		},
		{
			name: "targets",
			path: "/v1/node-targets",
			body: fmt.Sprintf(`{"node":%q}`, foreign),
			what: "read another node's profiling targets",
		},
		{
			name: "inventory",
			path: "/v1/node-inventory",
			body: fmt.Sprintf(`{"node":%q,"binaries":[],"counters":{}}`, foreign),
			what: "file Go-inventory facts against another node",
		},
		{
			name: "profile",
			path: "/v1/node-profile",
			body: fmt.Sprintf(`{"node":%q,"pod_uid":"abc","container_id":"def","pid":1,`+
				`"capture_start":"2026-08-26T10:00:00Z","capture_end":"2026-08-26T10:01:00Z","pprof":"AQID"}`, foreign),
			what: "file a captured profile against a workload on another node",
		},
	}
	for _, c := range cases {
		t.Run(c.name+" refuses a foreign node", func(t *testing.T) {
			status := probeReceiver(ctx, t, config, clientset, ns, probePod, endpoint+c.path, "", c.body)
			if status != 403 {
				t.Errorf("%s = %d, want 403 — a real node token could %s", c.path, status, c.what)
			}
		})
	}

	// Every refusal above must be the handler's, not the mux's. An unmounted
	// route answers 404 to everything, which looks like a check that is working
	// and is a check that never ran — this suite reported exactly that once,
	// before the install moved to the profile that mounts all four.
	t.Run("every refusal came from a route that exists", func(t *testing.T) {
		for _, c := range cases {
			status := probeReceiver(ctx, t, config, clientset, ns, probePod,
				endpoint+c.path, "", fmt.Sprintf(`{"node":%q}`, ownNode))
			if status == 404 {
				t.Errorf("%s is not mounted; its 403 above was the mux answering, not the handler", c.path)
			}
		}
	})
}

// TestATokenThatWasNotProjectedIntoAPodIsRefused covers the other half of the
// identity: a token minted through the TokenRequest API with no bound object
// carries the right subject and the right audience, and no node claim at all.
// Accepting it would mean accepting a caller whose node is whatever it says.
func TestATokenThatWasNotProjectedIntoAPodIsRefused(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	if agentImage == "" {
		t.Skip("E2E_AGENT_IMAGE not set; run `make identity-e2e`")
	}
	config := clusterConfig(t)
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	ns := uniqueNamespace(ctx, t, clientset, "runtime-agent-unbound-e2e")
	installChart(ctx, t, clientset, ns, agentImage, installOptions{
		profile:    "inventory",
		mutateNode: addChannelProbe,
	})
	waitDeploymentPod(ctx, t, clientset, ns, "controller")
	probePod := waitDaemonSetPod(ctx, t, clientset, ns)
	ownNode := podNode(ctx, t, clientset, ns, probePod)

	// Minted for the node's own ServiceAccount, with the audience the receiver
	// requires — everything the old check looked at, and nothing the kubelet
	// adds when it projects a token into a pod.
	tr, err := clientset.CoreV1().ServiceAccounts(ns).CreateToken(ctx, "runtime-agent-node",
		&authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{Audiences: []string{"rebuildstack-controller"}},
		}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("minting an unbound token: %v", err)
	}

	endpoint := fmt.Sprintf("http://runtime-agent-controller.%s.svc:8080/v1/node-scope", ns)
	status := probeReceiver(ctx, t, config, clientset, ns, probePod, endpoint,
		tr.Status.Token, fmt.Sprintf(`{"node":%q}`, ownNode))
	if status != 401 {
		t.Errorf("unbound token = %d, want 401 — a token with no node claim establishes no node", status)
	}
}

// addChannelProbe adds a shell sidecar that shares the node pod's projected
// token, so the suite can speak to the receiver as the node without
// reimplementing the node.
//
// It is the node's *own* mounted token, not a copy the test minted: what is
// being tested is what the kubelet writes into it.
func addChannelProbe(ds *appsv1.DaemonSet) {
	spec := &ds.Spec.Template.Spec
	spec.Containers = append(spec.Containers, corev1.Container{
		Name:            probeContainer,
		Image:           spoolReaderImage(),
		ImagePullPolicy: corev1.PullNever,
		Command:         []string{"sleep", "86400"},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      "controller-token",
			MountPath: "/var/run/secrets/rebuildstack.co/controller-token",
			ReadOnly:  true,
		}},
	})
}

// statusLine matches the status of the first response busybox wget reports.
var statusLine = regexp.MustCompile(`HTTP/1\.[01] (\d{3})`)

// probeReceiver POSTs body to url from the probe sidecar and returns the HTTP
// status. An empty token uses the node's own mounted one.
func probeReceiver(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface,
	ns, pod, url, token, body string,
) int {
	t.Helper()

	auth := `"Authorization: Bearer $(cat /var/run/secrets/rebuildstack.co/controller-token/token)"`
	if token != "" {
		auth = fmt.Sprintf(`"Authorization: Bearer %s"`, token)
	}
	// -S puts the status line on stderr, and a non-2xx also makes wget exit
	// non-zero; both are folded into stdout so one read carries the answer.
	script := fmt.Sprintf(`wget -q -S -O /dev/null -T 20 --header %s --header "Content-Type: application/json" --post-data %q %q 2>&1; true`,
		auth, body, url)

	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: probeContainer,
			Command:   []string{"sh", "-c", script},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		t.Fatalf("building exec: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		t.Fatalf("probing %s: %v\nstdout: %s\nstderr: %s", url, err, stdout.String(), stderr.String())
	}
	out := stdout.String() + stderr.String()
	m := statusLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no HTTP status in the probe output for %s:\n%s", url, out)
	}
	var status int
	if _, err := fmt.Sscanf(m[1], "%d", &status); err != nil {
		t.Fatalf("parsing status %q: %v", m[1], err)
	}
	return status
}

// podNode reports which node a pod was scheduled on — the value its projected
// token should carry, and the only node it may speak for.
func podNode(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, pod string) string {
	t.Helper()
	p, err := cs.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading pod %s: %v", pod, err)
	}
	if p.Spec.NodeName == "" {
		t.Fatalf("pod %s has no node", pod)
	}
	return p.Spec.NodeName
}

// uniqueNamespace names a namespace for this run and creates it.
func uniqueNamespace(ctx context.Context, t *testing.T, cs kubernetes.Interface, prefix string) string {
	t.Helper()
	ns := fmt.Sprintf("%s-%d", prefix, os.Getpid())
	createNamespace(ctx, t, cs, ns)
	return ns
}
