//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

// The listing is the one surface that names an object the filters excluded, and
// two claims are made about it (ADR 0084): it answers with the object and the
// control that refused it, and it answers only to a reader inside the pod.
//
// Both are properties of a deployed agent rather than of a handler — the first
// runs through a real API server's owner chains and namespace annotations, the
// second through whatever the chart actually binds — so neither is settled by
// the unit tests. Gated on E2E_AGENT_IMAGE; use `make collection-e2e`.
func TestTheCollectionListingNamesWhatWasExcluded(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	if agentImage == "" {
		t.Skip("E2E_AGENT_IMAGE not set; run `make collection-e2e`")
	}
	clientset := clusterClient(t)
	config := clusterConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-collection-e2e-%d", os.Getpid())
	fixtureNS := ns + "-fixture"
	createNamespace(ctx, t, clientset, ns)
	createNamespace(ctx, t, clientset, fixtureNS)

	// One pod the filters admit, one that opted itself out. The allow list
	// names the fixture namespace alone, so every other namespace in the
	// cluster is refused by the configuration — the third answer the listing
	// has to be able to give.
	makePod := func(name string, annotations map[string]string) {
		t.Helper()
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: fixtureNS, Name: name, Annotations: annotations},
			Spec: corev1.PodSpec{
				Containers:    []corev1.Container{{Name: "main", Image: pauseImage}},
				RestartPolicy: corev1.RestartPolicyNever,
			},
		}
		if _, err := clientset.CoreV1().Pods(fixtureNS).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating pod %s/%s: %v", fixtureNS, name, err)
		}
	}
	makePod("visible", nil)
	makePod("self-opted", map[string]string{collector.CollectAnnotation: "false"})

	installChart(ctx, t, clientset, ns, agentImage, installOptions{
		spoolReader: true,
		values: map[string]any{
			"filters": map[string]any{
				"namespaces": map[string]any{"allow": []any{fixtureNS}},
			},
		},
	})
	controller := waitDeploymentPod(ctx, t, clientset, ns, "controller")
	t.Logf("controller pod: %s", controller)

	list := waitForListing(ctx, t, config, clientset, ns, controller, fixtureNS)
	for key, d := range list {
		if !d.Collected {
			t.Logf("excluded: %s by %s", key, d.Reason)
		}
	}
	for _, want := range []struct {
		kind, namespace, name string
		collected             bool
		reason                collector.ExclusionReason
	}{
		{"Pod", fixtureNS, "visible", true, ""},
		{"Pod", fixtureNS, "self-opted", false, collector.ExcludedByPodAnnotation},
		{"Namespace", "", fixtureNS, true, ""},
		{"Namespace", "", "kube-system", false, collector.ExcludedByNamespaceFilter},
	} {
		got, ok := list[want.kind+"/"+want.namespace+"/"+want.name]
		if !ok {
			t.Errorf("the listing says nothing about %s %s/%s", want.kind, want.namespace, want.name)
			continue
		}
		if got.Collected != want.collected || got.Reason != want.reason {
			t.Errorf("%s %s/%s = collected %v reason %q, want %v and %q",
				want.kind, want.namespace, want.name,
				got.Collected, got.Reason, want.collected, want.reason)
		}
	}

	checkTheListingIsPodLocal(ctx, t, config, clientset, ns, fixtureNS, controller)
}

// waitForListing reads the listing from inside the controller pod until it
// carries both fixture pods, and returns it keyed by object.
func waitForListing(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface,
	ns, controller, fixtureNS string,
) map[string]collector.Decision {
	t.Helper()
	// The sidecar shares the pod's network namespace, which is the whole of
	// what "loopback" grants: same pod, no port, no policy, no credential.
	cmd := []string{"wget", "-q", "-O", "-", "-T", "20", "http://127.0.0.1:9091/collection"}

	deadline := time.Now().Add(4 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		out, ok := execSpoolReader(ctx, t, config, cs, ns, controller, cmd)
		if ok {
			last = out
			list := parseListing(t, out)
			if _, ok := list["Pod/"+fixtureNS+"/visible"]; ok {
				if _, ok := list["Pod/"+fixtureNS+"/self-opted"]; ok {
					t.Logf("the listing carries %d objects", len(list))
					return list
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("the listing never carried both fixture pods; last answer:\n%s", tail(last, 2000))
	return nil
}

func parseListing(t *testing.T, body string) map[string]collector.Decision {
	t.Helper()
	out := make(map[string]collector.Decision)
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" {
			continue
		}
		var d collector.Decision
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("the listing wrote a line that is not an object: %q: %v", line, err)
		}
		out[d.Kind+"/"+d.Namespace+"/"+d.Name] = d
	}
	return out
}

// checkTheListingIsPodLocal asserts the claim the bind address is there to make:
// a pod that is not this one cannot read it. Without that, the names of excluded
// objects would be readable by anything in the cluster that can reach the pod.
func checkTheListingIsPodLocal(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface,
	ns, fixtureNS, controller string,
) {
	t.Helper()
	pod, err := cs.CoreV1().Pods(ns).Get(ctx, controller, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the controller pod: %v", err)
	}
	if pod.Status.PodIP == "" {
		t.Fatal("the controller pod has no address to try")
	}

	caller := "collection-caller"
	_, err = cs.CoreV1().Pods(fixtureNS).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fixtureNS, Name: caller,
			// This pod exists to fail a connection, and collecting it would put
			// it in the listing it is trying to read.
			Annotations: map[string]string{collector.CollectAnnotation: "false"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "main", Image: spoolReaderImage(), Command: []string{"sleep", "600"},
			}},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the caller pod: %v", err)
	}
	waitPodRunning(ctx, t, cs, fixtureNS, caller)

	url := fmt.Sprintf("http://%s:9091/collection", pod.Status.PodIP)
	out, ok := execIn(ctx, t, config, cs, fixtureNS, caller, "main",
		[]string{"sh", "-c", fmt.Sprintf("wget -q -O - -T 8 %q 2>&1; echo exit=$?", url)})
	if !ok {
		t.Fatal("the caller pod could not be reached to run the probe")
	}
	t.Logf("a pod in %s asking %s: %s", fixtureNS, url, strings.TrimSpace(out))

	if strings.Contains(out, "exit=0") {
		t.Errorf("another pod read the listing at %s; it names objects the filters excluded:\n%s", url, out)
	}
	if strings.Contains(out, fixtureNS) {
		t.Errorf("the refused answer carried a cluster object:\n%s", out)
	}
}

// execIn runs cmd in one container of one pod and returns its stdout. A non-zero
// exit returns ok=false, so a caller polling for something not written yet
// retries rather than failing.
func execIn(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface,
	ns, pod, container string, cmd []string,
) (string, bool) {
	t.Helper()
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		t.Fatalf("building exec: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return "", false
	}
	return stdout.String(), true
}
