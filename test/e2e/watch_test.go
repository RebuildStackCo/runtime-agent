//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The two halves of ADR 0035, staged against a real API server because the
// thing under test is what the API server does with a permission that changes
// while a client is connected — which no fake reproduces.
//
// Both tests take minutes rather than seconds, and the reason is the mechanism
// itself: an established watch is not re-authorized, so a revoked grant is
// noticed when client-go next re-establishes the connection, which it does on a
// timeout of its own choosing between five and ten minutes.

// TestAnAgentDeniedAGatingPermissionStopsInsteadOfWaitingForever installs the
// agent without the `pods` grant.
//
// Before ADR 0035 this produced the worst outcome available: WaitForCacheSync
// has no timeout, so the process sat in it forever — a Running pod, no data, no
// error. Now the agent exits naming the resource, which puts the pod in
// CrashLoopBackOff where a customer can see it. Use `make watch-e2e`.
func TestAnAgentDeniedAGatingPermissionStopsInsteadOfWaitingForever(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	if agentImage == "" {
		t.Skip("E2E_AGENT_IMAGE not set; run `make watch-e2e`")
	}
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-watch-deny-e2e-%d", os.Getpid())
	createNamespace(ctx, t, clientset, ns)

	installChart(ctx, t, clientset, ns, agentImage, installOptions{
		narrowRole: func(cr *rbacv1.ClusterRole) { removeResource(cr, "", "pods") },
	})
	pod := waitDeploymentPod(ctx, t, clientset, ns, "controller")
	t.Logf("controller pod: %s (installed without the pods grant)", pod)

	// The watchdog waits out a five-minute streak before it acts, deliberately:
	// a restart costs the in-memory windows and the spool, so a brief API server
	// hiccup must not buy one. Allow for that plus a scheduling round.
	restarts := waitForControllerRestart(ctx, t, clientset, ns, pod, 11*time.Minute)
	t.Logf("the controller restarted %d time(s)", restarts)

	logs := previousControllerLogs(ctx, t, clientset, ns, pod)
	for _, want := range []string{"pods", "failing continuously"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the exit did not say %q; a crash that does not name the grant is not an improvement:\n%s",
				want, tail(logs, 2000))
		}
	}
}

// TestAPermissionRevokedFromTheRunningAgentReachesThePayload is the case
// ADR 0033 §5 recorded as undetected: a grant taken from an agent that synced.
//
// HasSynced is a one-way latch, so before ADR 0035 the cache went on answering
// from what it last held and `workload_policy` went on declaring every source
// read. The claim is that the payload now says it cannot see budgets, while
// everything the revocation did not touch keeps being collected. Gated on
// E2E_AGENT_IMAGE and E2E_SPOOL_READER_IMAGE; use `make watch-e2e`.
func TestAPermissionRevokedFromTheRunningAgentReachesThePayload(t *testing.T) {
	agentImage := os.Getenv("E2E_AGENT_IMAGE")
	if agentImage == "" {
		t.Skip("E2E_AGENT_IMAGE not set; run `make watch-e2e`")
	}
	config := clusterConfig(t)
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	ns := fmt.Sprintf("runtime-agent-watch-revoke-e2e-%d", os.Getpid())
	fixtureNS := ns + "-fixtures"
	createNamespace(ctx, t, clientset, ns)
	createNamespace(ctx, t, clientset, fixtureNS)
	createBudgetFixture(ctx, t, clientset, fixtureNS)

	deployController(ctx, t, clientset, ns, agentImage)
	pod := waitDeploymentPod(ctx, t, clientset, ns, "controller")
	t.Logf("controller pod: %s", pod)

	// First the agent must actually read budgets, or the second half proves
	// nothing: a source that was never available cannot become unavailable.
	waitForPolicySources(ctx, t, config, clientset, ns, pod, 5*time.Minute,
		"the budget cache to fill", func(sources []string) bool { return len(sources) == 0 })
	t.Log("budgets are readable and the payload declares nothing")

	revokeBudgetGrant(ctx, t, clientset, ns)
	t.Log("the poddisruptionbudgets grant is revoked from the running agent")

	// The established watch is not re-authorized; client-go re-establishes it on
	// its own five-to-ten-minute timeout, and the refusal lands then.
	waitForPolicySources(ctx, t, config, clientset, ns, pod, 13*time.Minute,
		"the revoked source to be declared", func(sources []string) bool {
			for _, s := range sources {
				if s == "pod_disruption_budgets" {
					return true
				}
			}
			return false
		})

	// One payload degraded, the agent untouched: the split ADR 0033 §1 drew
	// between a cache that gates a signal and one that adds it.
	if restarts := controllerRestarts(ctx, t, clientset, ns, pod); restarts != 0 {
		t.Errorf("the controller restarted %d time(s); a policy source must degrade a payload, not the agent", restarts)
	}
	raw, ok := readSpoolFile(ctx, t, config, clientset, ns, pod, workloadMetadataSpoolPath)
	if !ok {
		t.Fatal("workload metadata stopped being written when one policy grant was revoked")
	}
	var metadata struct {
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		t.Fatalf("decoding workload metadata: %v", err)
	}
	if len(metadata.Records) == 0 {
		t.Error("workload metadata is empty: the revocation cost more than the payload that reads budgets")
	}
}

// removeResource drops one resource from every rule of the role that names it,
// leaving the rest of the grant intact — the shape of a customer editing a
// ClusterRole they do not fully agree with.
func removeResource(cr *rbacv1.ClusterRole, group, resource string) {
	rules := cr.Rules[:0]
	for _, rule := range cr.Rules {
		if containsString(rule.APIGroups, group) {
			kept := make([]string, 0, len(rule.Resources))
			for _, r := range rule.Resources {
				if r != resource {
					kept = append(kept, r)
				}
			}
			rule.Resources = kept
		}
		// A rule granting nothing is not a narrower grant, it is invalid: the
		// API server requires at least one resource. Dropping the rule is what a
		// customer removing the last resource of one would have to do anyway.
		if len(rule.Resources) == 0 && len(rule.NonResourceURLs) == 0 {
			continue
		}
		rules = append(rules, rule)
	}
	cr.Rules = rules
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// revokeBudgetGrant removes poddisruptionbudgets from the deployed ClusterRole
// while the agent holds an open watch on them.
func revokeBudgetGrant(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns string) {
	t.Helper()
	name := controllerClusterRoleName(ns)
	role, err := cs.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading ClusterRole %s: %v", name, err)
	}
	removeResource(role, "policy", "poddisruptionbudgets")
	if _, err := cs.RbacV1().ClusterRoles().Update(ctx, role, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("revoking the budget grant: %v", err)
	}
}

// waitForPolicySources polls the workload-policy payload until its declared
// sources satisfy want.
func waitForPolicySources(ctx context.Context, t *testing.T, config *rest.Config, cs kubernetes.Interface, ns, pod string, within time.Duration, what string, want func([]string) bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last []string
	for time.Now().Before(deadline) {
		raw, ok := readSpoolFile(ctx, t, config, cs, ns, pod, workloadPolicySpoolPath)
		if ok {
			var payload struct {
				UnavailableSources []string `json:"unavailable_sources"`
			}
			if err := json.Unmarshal([]byte(raw), &payload); err == nil {
				last = payload.UnavailableSources
				if want(last) {
					return
				}
			}
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("timed out waiting for %s; last declared sources = %v", what, last)
}

// waitForControllerRestart waits for the agent container to have exited at
// least once and returns the restart count.
func waitForControllerRestart(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, pod string, within time.Duration) int32 {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if n := controllerRestarts(ctx, t, cs, ns, pod); n > 0 {
			return n
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("the agent was still running %s after being denied the pods grant; it is waiting on a sync that will never come", within)
	return 0
}

// controllerRestarts is the agent container's restart count, 0 when the pod is
// gone (a pod that no longer exists cannot be said to have restarted).
func controllerRestarts(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, pod string) int32 {
	t.Helper()
	p, err := cs.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return 0
	}
	for _, status := range p.Status.ContainerStatuses {
		if status.Name == "controller" {
			return status.RestartCount
		}
	}
	return 0
}

// previousControllerLogs returns the log of the agent container's previous
// incarnation — the one that exited.
func previousControllerLogs(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, pod string) string {
	t.Helper()
	stream, err := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: "controller",
		Previous:  true,
	}).Stream(ctx)
	if err != nil {
		t.Fatalf("reading the previous controller log: %v", err)
	}
	defer func() { _ = stream.Close() }()
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("reading the previous controller log: %v", err)
	}
	return string(out)
}

// createNamespace creates ns and schedules its removal.
func createNamespace(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns string) {
	t.Helper()
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating namespace %s: %v", ns, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_ = cs.CoreV1().Namespaces().Delete(cleanupCtx, ns, metav1.DeleteOptions{})
	})
}

// createBudgetFixture gives the workload-policy payload something to read, so
// "sources all read" is a claim about a cache that actually holds an object.
func createBudgetFixture(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns string) {
	t.Helper()
	minAvailable := intstr.FromString("50%")
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-pdb"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		},
	}
	if _, err := cs.PolicyV1().PodDisruptionBudgets(ns).Create(ctx, pdb, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the budget fixture: %v", err)
	}
}

// tail returns the last n bytes of s, for log excerpts in failure messages.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
