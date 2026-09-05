package chartrender_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/RebuildStackCo/runtime-agent/internal/chartrender"
	"github.com/RebuildStackCo/runtime-agent/internal/config"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

// The chart is the only installer, so a mistake in it is a mistake in the
// product — a missing grant, a config key the agent rejects at startup — and
// none of it is visible in a diff of Go code. So the promises docs/security.md
// makes about an installation are asserted here against the rendered manifests,
// for every profile. The absences are in guardrail_test.go.

// chartDir is the chart's path from this package.
const chartDir = "../../" + chartrender.Dir

// The port both roles answer probes on, spelled out here rather than read from
// the chart: a test that takes its expectation from the thing it tests asserts
// nothing. Changing it means changing this line (ADR 0069).
const (
	healthPort     int32 = 9090
	healthPortName       = "health"
)

// ebpfValues is the smallest values overlay that renders the profiler, since
// the chart refuses to render it with an empty allow-list.
func ebpfValues() map[string]any {
	return map[string]any{
		"profile": "ebpf",
		"profiling": map[string]any{
			"allowedModulePrefixes": []any{"github.com/acme/"},
		},
	}
}

func profiles() map[string]map[string]any {
	return map[string]map[string]any{
		"metrics-only": {"profile": "metrics-only"},
		"inventory":    {"profile": "inventory"},
		"ebpf":         ebpfValues(),
	}
}

func render(t *testing.T, values map[string]any) []string {
	t.Helper()
	docs, err := chartrender.Manifests(chartDir, chartrender.Options{
		ReleaseName: "rs",
		Namespace:   "observability",
		Values:      values,
	})
	if err != nil {
		t.Fatalf("rendering chart: %v", err)
	}
	return docs
}

// decode returns the documents of the given kind, decoded into T.
func decode[T any](t *testing.T, docs []string, kind string) []T {
	t.Helper()
	var out []T
	for _, doc := range docs {
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatalf("decoding TypeMeta: %v\n%s", err, doc)
		}
		if meta.Kind != kind {
			continue
		}
		var obj T
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("decoding %s: %v\n%s", kind, err, doc)
		}
		out = append(out, obj)
	}
	return out
}

func only[T any](t *testing.T, objs []T, what string) T {
	t.Helper()
	if len(objs) != 1 {
		t.Fatalf("found %d %s, want exactly one", len(objs), what)
	}
	return objs[0]
}

// The promise of docs/security.md §1: the agent holds no write verb on any
// Kubernetes resource. Asserted against the bytes, in every profile, because a
// verb is one word and review is not a mechanism.
func TestTheClusterRoleGrantsNoWriteVerb(t *testing.T) {
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			role := only(t, decode[rbacv1.ClusterRole](t, render(t, values), "ClusterRole"), "ClusterRole")
			for _, rule := range role.Rules {
				for _, verb := range rule.Verbs {
					switch verb {
					case "get", "list", "watch":
					default:
						t.Errorf("rule %v grants %q; the agent is read-only (docs/security.md §1)",
							rule.Resources, verb)
					}
				}
			}
		})
	}
}

// Every cache the agent needs must be granted. Since ADR 0035 a missing gating
// grant stops the agent outright, so this test is the difference between
// catching a chart mistake in CI and catching it as a CrashLoopBackOff in a
// customer's cluster.
func TestTheClusterRoleGrantsEveryCacheTheAgentOpens(t *testing.T) {
	want := map[string][]string{
		"":                  {"pods", "namespaces", "nodes", "services", "limitranges", "resourcequotas", "persistentvolumeclaims"},
		"apps":              {"replicasets", "deployments", "statefulsets", "daemonsets"},
		"batch":             {"jobs", "cronjobs"},
		"policy":            {"poddisruptionbudgets"},
		"autoscaling":       {"horizontalpodautoscalers"},
		"scheduling.k8s.io": {"priorityclasses"},
		"storage.k8s.io":    {"storageclasses"},
	}
	role := only(t, decode[rbacv1.ClusterRole](t, render(t, map[string]any{}), "ClusterRole"), "ClusterRole")
	granted := map[string]map[string]bool{}
	for _, rule := range role.Rules {
		for _, group := range rule.APIGroups {
			if granted[group] == nil {
				granted[group] = map[string]bool{}
			}
			for _, resource := range rule.Resources {
				granted[group][resource] = true
			}
		}
	}
	for group, resources := range want {
		for _, resource := range resources {
			if !granted[group][resource] {
				t.Errorf("no grant for %q in apiGroup %q; the agent opens an informer on it and ADR 0035 makes that fatal", resource, group)
			}
		}
	}
}

// The node role's defining property (ADR 0009): its ServiceAccount is named by
// no binding anywhere, and no API token is placed in the container. The `ebpf`
// profile adds kernel capabilities and must not disturb either.
func TestTheNodeHoldsNoAPICredential(t *testing.T) {
	for _, name := range []string{"inventory", "ebpf"} {
		t.Run(name, func(t *testing.T) {
			docs := render(t, profiles()[name])
			ds := only(t, decode[appsv1.DaemonSet](t, docs, "DaemonSet"), "DaemonSet")
			nodeSA := ds.Spec.Template.Spec.ServiceAccountName

			automount := ds.Spec.Template.Spec.AutomountServiceAccountToken
			if automount == nil || *automount {
				t.Error("the node pod mounts its API token; the node role must hold no API credential (ADR 0009)")
			}
			for _, binding := range decode[rbacv1.ClusterRoleBinding](t, docs, "ClusterRoleBinding") {
				for _, subject := range binding.Subjects {
					if subject.Kind == "ServiceAccount" && subject.Name == nodeSA {
						t.Errorf("ClusterRoleBinding %q names the node ServiceAccount; it must be bound to nothing", binding.Name)
					}
				}
			}
			for _, binding := range decode[rbacv1.RoleBinding](t, docs, "RoleBinding") {
				for _, subject := range binding.Subjects {
					if subject.Kind == "ServiceAccount" && subject.Name == nodeSA {
						t.Errorf("RoleBinding %q names the node ServiceAccount; it must be bound to nothing", binding.Name)
					}
				}
			}
		})
	}
}

// docs/security.md §7 promises no privileged container and a named, minimal set
// of capabilities. The `ebpf` profile's whole claim is that it adds exactly two.
func TestNoContainerIsPrivilegedAndCapabilitiesAreExactlyWhatIsPromised(t *testing.T) {
	cases := []struct {
		profile string
		values  map[string]any
		nodeCap []corev1.Capability
	}{
		{"inventory", profiles()["inventory"], []corev1.Capability{"SYS_PTRACE"}},
		{"ebpf", ebpfValues(), []corev1.Capability{"SYS_PTRACE", "BPF", "PERFMON"}},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			docs := render(t, tc.values)
			for _, container := range allContainers(t, docs) {
				sc := container.SecurityContext
				if sc == nil {
					t.Fatalf("container %q has no securityContext", container.Name)
				}
				if sc.Privileged != nil && *sc.Privileged {
					t.Errorf("container %q is privileged", container.Name)
				}
				if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
					t.Errorf("container %q allows privilege escalation", container.Name)
				}
				if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
					t.Errorf("container %q does not drop ALL capabilities", container.Name)
				}
			}
			ds := only(t, decode[appsv1.DaemonSet](t, docs, "DaemonSet"), "DaemonSet")
			got := ds.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Add
			if !sameCapabilities(got, tc.nodeCap) {
				t.Errorf("node capabilities = %v, want exactly %v (docs/security.md §7)", got, tc.nodeCap)
			}
		})
	}
}

// No host path is ever writable, in any profile, in any pod the chart renders.
// This is the difference between reading a node and being able to change one.
func TestEveryHostMountIsReadOnly(t *testing.T) {
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			for _, pod := range allPodSpecs(t, render(t, values)) {
				hostPaths := map[string]bool{}
				for _, volume := range pod.spec.Volumes {
					if volume.HostPath != nil {
						hostPaths[volume.Name] = true
					}
				}
				for _, container := range pod.containers() {
					for _, mount := range container.VolumeMounts {
						if hostPaths[mount.Name] && !mount.ReadOnly {
							t.Errorf("%s: host mount %q is writable in %q", pod, mount.Name, container.Name)
						}
					}
				}
			}
		})
	}
}

// ADR 0026: the spool is an emptyDir, always, and no value changes it. A
// persistence knob would have to be honoured by the agent, which has no code
// for it, so the knob's absence is the feature.
func TestTheSpoolIsAlwaysAnEmptyDir(t *testing.T) {
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			deploy := only(t, decode[appsv1.Deployment](t, render(t, values), "Deployment"), "Deployment")
			var found bool
			for _, volume := range deploy.Spec.Template.Spec.Volumes {
				if volume.Name != "spool" {
					continue
				}
				found = true
				if volume.EmptyDir == nil {
					t.Errorf("the spool volume is not an emptyDir (ADR 0026)")
					continue
				}
				// An emptyDir with no sizeLimit draws on the node's whole
				// ephemeral storage, and a node that runs out makes the kubelet
				// evict pods — other people's pods (ADR 0042). The agent bounds
				// its own spool; this is what holds if that ever fails, so it
				// must be larger than the agent's budget and must exist.
				limit := volume.EmptyDir.SizeLimit
				if limit == nil {
					t.Errorf("the spool emptyDir has no sizeLimit; it can consume the node's ephemeral storage")
					continue
				}
				if limit.Value() <= sink.DefaultMaxBytes {
					t.Errorf("spool sizeLimit %s is not above the agent's own %d-byte budget; "+
						"the agent's bound is the one meant to act",
						limit.String(), int64(sink.DefaultMaxBytes))
				}
			}
			if !found {
				t.Error("no spool volume")
			}
			if deploy.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
				t.Errorf("strategy = %q, want Recreate: one replica must mean at-most-one (ADR 0026)",
					deploy.Spec.Strategy.Type)
			}
			if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 1 {
				t.Errorf("replicas = %v, want 1 (ADR 0008)", deploy.Spec.Replicas)
			}
		})
	}
}

// The rendered configuration is fed to the agent's own parser, which rejects
// unknown keys. Without this, a chart that renders a misspelled key ships, and
// the failure appears as a CrashLoopBackOff in a customer's cluster with the
// chart looking innocent.
func TestTheRenderedConfigurationParsesAsTheAgentWillParseIt(t *testing.T) {
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			docs := render(t, values)
			for _, cm := range decode[corev1.ConfigMap](t, docs, "ConfigMap") {
				body, ok := cm.Data["config.yaml"]
				if !ok {
					t.Fatalf("ConfigMap %q carries no config.yaml", cm.Name)
				}
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatalf("writing config: %v", err)
				}
				if strings.HasSuffix(cm.Name, "-node") {
					if _, err := config.LoadNode(path); err != nil {
						t.Errorf("the node config this chart renders does not parse: %v\n%s", err, body)
					}
					continue
				}
				if _, err := config.Load(path); err != nil {
					t.Errorf("the controller config this chart renders does not parse: %v\n%s", err, body)
				}
			}
		})
	}
}

// Three strings had to agree by hand before the chart existed: the audience the
// node's token requests, the audience the controller requires, and the subject
// the controller pins — which carries the namespace. Installing anywhere but
// `runtime-agent` silently broke the third. The chart derives all three, and
// this renders into a different namespace to prove it.
func TestTheNodeTokenAndTheControllerAgreeInAnyNamespace(t *testing.T) {
	docs := render(t, ebpfValues()) // rendered into namespace "observability"

	var controllerCfg config.Config
	for _, cm := range decode[corev1.ConfigMap](t, docs, "ConfigMap") {
		if !strings.HasSuffix(cm.Name, "-node") {
			controllerCfg = loadControllerConfig(t, cm)
		}
	}

	ds := only(t, decode[appsv1.DaemonSet](t, docs, "DaemonSet"), "DaemonSet")
	var tokenAudience string
	for _, volume := range ds.Spec.Template.Spec.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil {
				tokenAudience = source.ServiceAccountToken.Audience
			}
		}
	}

	if tokenAudience != controllerCfg.NodeIntake.Audience {
		t.Errorf("the node requests audience %q and the controller requires %q",
			tokenAudience, controllerCfg.NodeIntake.Audience)
	}
	wantSubject := "system:serviceaccount:observability:" + ds.Spec.Template.Spec.ServiceAccountName
	if controllerCfg.NodeIntake.ExpectedSubject != wantSubject {
		t.Errorf("the controller pins subject %q, but the node runs as %q",
			controllerCfg.NodeIntake.ExpectedSubject, wantSubject)
	}
	if !strings.Contains(strings.Join(ds.Spec.Template.Spec.Containers[0].Args, " "), ".observability.svc") {
		t.Errorf("the node's controller endpoint does not point at the release namespace: %v",
			ds.Spec.Template.Spec.Containers[0].Args)
	}
}

// A profile is a claim about what is installed, so the absences have to be real:
// a controller-only install receives nothing and is reachable by nothing. The
// health port is the one exception and it is named, not tolerated — it carries
// no collected data and answers only about this process (ADR 0069).
func TestMetricsOnlyInstallsNoNodeAndOpensOnlyItsHealthPort(t *testing.T) {
	docs := render(t, map[string]any{"profile": "metrics-only"})
	if ds := decode[appsv1.DaemonSet](t, docs, "DaemonSet"); len(ds) != 0 {
		t.Error("metrics-only rendered a DaemonSet")
	}
	if svc := decode[corev1.Service](t, docs, "Service"); len(svc) != 0 {
		t.Error("metrics-only rendered a Service; nothing sends it reports")
	}
	deploy := only(t, decode[appsv1.Deployment](t, docs, "Deployment"), "Deployment")
	ports := deploy.Spec.Template.Spec.Containers[0].Ports
	if len(ports) != 1 || ports[0].Name != healthPortName || ports[0].ContainerPort != healthPort {
		t.Errorf("metrics-only opens %v, want only the health port %d", ports, healthPort)
	}
	if sa := decode[corev1.ServiceAccount](t, docs, "ServiceAccount"); len(sa) != 1 {
		t.Errorf("metrics-only rendered %d ServiceAccounts, want only the controller's", len(sa))
	}
	if np := decode[networkingv1.NetworkPolicy](t, docs, "NetworkPolicy"); len(np) != 0 {
		t.Error("metrics-only rendered a NetworkPolicy; there is no port to restrict")
	}
}

// Every pod the chart renders carries all three probes, in every profile.
//
// The two states an install had before them: a controller wedged holding a lock
// is never restarted, and `kubectl rollout status` returns success while the
// caches are still filling, because a pod with no readiness probe is Ready as
// soon as its process exists (ADR 0069). The image has no shell (ADR 0037), so
// asserting the scheme is asserting the probe can run at all.
func TestEveryPodSaysWhetherItIsAliveAndWhetherItIsReady(t *testing.T) {
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			for _, pod := range allPodSpecs(t, render(t, values)) {
				for _, container := range pod.containers() {
					probes := map[string]*corev1.Probe{
						"startupProbe":   container.StartupProbe,
						"livenessProbe":  container.LivenessProbe,
						"readinessProbe": container.ReadinessProbe,
					}
					for kind, probe := range probes {
						if probe == nil {
							t.Errorf("%s: container %q has no %s", pod, container.Name, kind)
							continue
						}
						if probe.HTTPGet == nil {
							t.Errorf("%s: container %q %s is not an httpGet; the image has no shell (ADR 0037)",
								pod, container.Name, kind)
							continue
						}
						if got := probe.HTTPGet.Port.IntValue(); got != int(healthPort) {
							t.Errorf("%s: container %q %s asks port %v, want the health port %d",
								pod, container.Name, kind, probe.HTTPGet.Port, healthPort)
						}
					}
					// Liveness and readiness are different questions, and a
					// liveness probe pointed at readiness restarts the agent for
					// every reason readiness fails — an API server outage among
					// them (ADR 0069 §2).
					if container.LivenessProbe.HTTPGet.Path == container.ReadinessProbe.HTTPGet.Path {
						t.Errorf("%s: container %q asks the same path for liveness and readiness (%s)",
							pod, container.Name, container.LivenessProbe.HTTPGet.Path)
					}
					// The container must actually declare the port the probes
					// ask on, and the process must be told to open it.
					if got := containerPort(t, container, healthPortName); got != healthPort {
						t.Errorf("%s: container %q declares %s = %d, want %d", pod, container.Name, healthPortName, got, healthPort)
					}
				}
			}
		})
	}
}

// The address the probes ask on and the address the agent binds are configured
// through different mechanisms in the two roles — a config file for the
// controller, a flag for the node (ADR 0025, ADR 0069 §4) — so a change to one
// cannot be assumed to have reached the other.
func TestBothRolesAreToldToOpenThePortTheProbesAsk(t *testing.T) {
	want := fmt.Sprintf(":%d", healthPort)
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			docs := render(t, values)

			var controllerCfg config.Config
			for _, cm := range decode[corev1.ConfigMap](t, docs, "ConfigMap") {
				if strings.HasSuffix(cm.Name, "-node") {
					continue
				}
				if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &controllerCfg); err != nil {
					t.Fatalf("decoding the controller config: %v", err)
				}
			}
			if controllerCfg.Health.ListenAddress != want {
				t.Errorf("the controller is configured to listen on %q, want %q",
					controllerCfg.Health.ListenAddress, want)
			}

			for _, ds := range decode[appsv1.DaemonSet](t, docs, "DaemonSet") {
				args := ds.Spec.Template.Spec.Containers[0].Args
				if !hasFlagValue(args, "-health-address", want) {
					t.Errorf("the node is not told to listen on %q: %v", want, args)
				}
			}
		})
	}
}

// hasFlagValue reports whether args carries flag immediately followed by value.
func hasFlagValue(args []string, flag, value string) bool {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// NOTES.txt is a template Helm prints rather than applies. It reached the
// manifest list once and every decode in this file failed on it with a YAML
// parse error, which is a confusing way to learn that a non-manifest is being
// treated as one. Say it directly instead.
func TestTheInstallNotesAreNotAManifest(t *testing.T) {
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			for _, doc := range render(t, values) {
				if strings.Contains(doc, "runtime-agent is installed in namespace") {
					t.Fatalf("NOTES.txt is in the manifest list; it is printed to the installer, not applied:\n%s", doc)
				}
			}
		})
	}
}

// TestTheReceiverIsRestrictedToTheNodeDaemonSet is the policy three ADRs
// described as shipped for months while it did not exist (ADR 0039) — asserted
// rather than reviewed, because the failure was not a wrong policy but a
// believed one.
//
// Each assertion closes a silent hole: an absent policyTypes allows everything;
// a second or empty `from` peer reopens the port; a peer selecting nothing
// denies the node itself; a policy on the wrong port is a policy on nothing.
func TestTheReceiverIsRestrictedToTheNodeDaemonSet(t *testing.T) {
	for name, values := range profiles() {
		if name == "metrics-only" {
			continue // no node, no port, no policy — covered above
		}
		t.Run(name, func(t *testing.T) {
			docs := render(t, values)
			np := only(t, decode[networkingv1.NetworkPolicy](t, docs, "NetworkPolicy"), "NetworkPolicy")

			if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
				t.Errorf("policyTypes = %v, want exactly [Ingress]", np.Spec.PolicyTypes)
			}
			// The policy must select the controller, or it restricts a pod that
			// does not exist and the receiver stays open.
			deploy := only(t, decode[appsv1.Deployment](t, docs, "Deployment"), "Deployment")
			if !selects(np.Spec.PodSelector.MatchLabels, deploy.Spec.Template.Labels) {
				t.Errorf("podSelector %v does not select the controller pod %v",
					np.Spec.PodSelector.MatchLabels, deploy.Spec.Template.Labels)
			}

			// Two rules: the receiver, restricted to the node, and the health
			// port, restricted to nothing (ADR 0069 §5). A third is another way
			// in and has to be argued for here.
			if len(np.Spec.Ingress) != 2 {
				t.Fatalf("ingress has %d rules, want exactly 2 — every extra rule is another way in", len(np.Spec.Ingress))
			}
			rule := ruleForPort(t, np, 8080)
			if len(rule.From) != 1 {
				t.Fatalf("ingress rule has %d peers, want exactly 1; an empty or extra peer opens the port", len(rule.From))
			}
			peer := rule.From[0]
			if peer.PodSelector == nil || peer.NamespaceSelector != nil || peer.IPBlock != nil {
				t.Fatalf("ingress peer = %+v, want a bare podSelector in the release namespace", peer)
			}
			if len(peer.PodSelector.MatchLabels) == 0 {
				t.Fatal("ingress peer selects on no labels, which admits every pod in the namespace")
			}
			ds := only(t, decode[appsv1.DaemonSet](t, docs, "DaemonSet"), "DaemonSet")
			if !selects(peer.PodSelector.MatchLabels, ds.Spec.Template.Labels) {
				t.Errorf("ingress peer %v does not select the node pod %v — the node would be denied its own channel",
					peer.PodSelector.MatchLabels, ds.Spec.Template.Labels)
			}
			// And it must not select the controller: a selector loose enough to
			// match both components is loose enough to match a stranger.
			if selects(peer.PodSelector.MatchLabels, deploy.Spec.Template.Labels) {
				t.Error("ingress peer also selects the controller; the selector is too broad to mean anything")
			}

			if len(rule.Ports) != 1 {
				t.Fatalf("ingress rule names %d ports, want exactly 1", len(rule.Ports))
			}
			// The rule must name the port the receiver actually listens on, or
			// it is a policy on nothing.
			port := containerPort(t, deploy.Spec.Template.Spec.Containers[0], "node-intake")
			if rule.Ports[0].Port == nil || rule.Ports[0].Port.String() != strconv.Itoa(int(port)) {
				t.Errorf("ingress port = %v, want the receiver's %d", rule.Ports[0].Port, port)
			}

			// And the second rule opens the health port and only that: an
			// unrestricted rule naming the receiver's port would hand every pod
			// in the cluster the channel the first rule just closed.
			health := ruleForPort(t, np, healthPort)
			if len(health.From) != 0 {
				t.Errorf("the health rule names %d peers; the kubelet arrives from the node address and no selector here can name it", len(health.From))
			}
			if len(health.Ports) != 1 {
				t.Errorf("the health rule names %d ports, want only %d", len(health.Ports), healthPort)
			}
		})
	}
}

// ruleForPort returns the one ingress rule naming port, failing if none or more
// than one does.
func ruleForPort(t *testing.T, np networkingv1.NetworkPolicy, port int32) networkingv1.NetworkPolicyIngressRule {
	t.Helper()
	var found []networkingv1.NetworkPolicyIngressRule
	for _, rule := range np.Spec.Ingress {
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.String() == strconv.Itoa(int(port)) {
				found = append(found, rule)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d ingress rules name port %d, want exactly 1", len(found), port)
	}
	return found[0]
}

// containerPort returns the container port declared under name.
func containerPort(t *testing.T, c corev1.Container, name string) int32 {
	t.Helper()
	for _, p := range c.Ports {
		if p.Name == name {
			return p.ContainerPort
		}
	}
	t.Fatalf("container %q declares no port named %q", c.Name, name)
	return 0
}

// selects reports whether every label in selector is present, with the same
// value, in labels.
func selects(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return len(selector) > 0
}

// The profiler is off unless the profile asks for it, in both halves: the node
// carries no eBPF flag and the controller answers no targeting query.
func TestTheProfilerIsAbsentUnlessTheProfileAsksForIt(t *testing.T) {
	docs := render(t, map[string]any{"profile": "inventory"})
	ds := only(t, decode[appsv1.DaemonSet](t, docs, "DaemonSet"), "DaemonSet")
	for _, arg := range ds.Spec.Template.Spec.Containers[0].Args {
		if arg == "-enable-ebpf" {
			t.Error("the inventory profile enables the eBPF profiler")
		}
	}
	for _, cm := range decode[corev1.ConfigMap](t, docs, "ConfigMap") {
		if strings.HasSuffix(cm.Name, "-node") {
			t.Errorf("the inventory profile renders a node profiling config (%s)", cm.Name)
			continue
		}
		if loadControllerConfig(t, cm).Profiling.Enabled {
			t.Error("the inventory profile enables the controller's targeting endpoint")
		}
	}
}

// The chart used to refuse this, because an empty allow-list admitted no frame
// of a service under its own domain-bearing module: profiling ran and produced
// nothing, forever, silently. The node now reads each build's own modules from
// the binary, so the empty list is a working install rather than a trap, and
// requiring a value nobody needs is the inert knob ADR 0025 abolished
// (ADR 0059 §4).
func TestTheProfilerInstallsWithNothingConfigured(t *testing.T) {
	docs, err := chartrender.Manifests(chartDir, chartrender.Options{
		Values: map[string]any{"profile": "ebpf"},
	})
	if err != nil {
		t.Fatalf("the chart refuses an ebpf install with no allow-list: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("the ebpf profile rendered nothing")
	}
}

// A profile the agent cannot honour must fail rather than look honoured.
// "pprof" is in the list because it reads like a profile and is not one: pulling
// from `/debug/pprof` is part of `inventory`, not an alternative to it
// (ADR 0057).
func TestAProfileTheAgentCannotHonourIsRefused(t *testing.T) {
	for _, profile := range []string{"pprof", "everything"} {
		_, err := chartrender.Manifests(chartDir, chartrender.Options{
			Values: map[string]any{"profile": profile},
		})
		if err == nil {
			t.Errorf("the chart rendered profile %q", profile)
		}
	}
}

// Values are strict for the same reason the agent's config parser is: a typo
// must not silently disable a filter, and a knob that does not exist must not
// look accepted.
func TestUnknownValuesAreRejected(t *testing.T) {
	for _, values := range []map[string]any{
		{"spool": map[string]any{"persistence": true}},
		{"filters": map[string]any{"namespace": map[string]any{"allow": []any{"shop"}}}},
		{"backend": map[string]any{"endpoint": "https://example.com"}},
	} {
		if _, err := chartrender.Manifests(chartDir, chartrender.Options{Values: values}); err == nil {
			t.Errorf("the chart accepted unknown values %v", values)
		}
	}
}

// loadControllerConfig parses a rendered ConfigMap the way the agent will.
func loadControllerConfig(t *testing.T, cm corev1.ConfigMap) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(cm.Data["config.yaml"]), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("parsing the controller config this chart renders: %v\n%s", err, cm.Data["config.yaml"])
	}
	return cfg
}

// podSpec is one rendered pod template with enough identity to name it in a
// failure. Every workload the chart renders is one of these, so a test written
// against allPodSpecs cannot miss a component the chart grows later.
type podSpec struct {
	kind string // Deployment or DaemonSet
	name string
	spec corev1.PodSpec
}

func (p podSpec) String() string { return p.kind + " " + p.name }

func allPodSpecs(t *testing.T, docs []string) []podSpec {
	t.Helper()
	var out []podSpec
	for _, deploy := range decode[appsv1.Deployment](t, docs, "Deployment") {
		out = append(out, podSpec{"Deployment", deploy.Name, deploy.Spec.Template.Spec})
	}
	for _, ds := range decode[appsv1.DaemonSet](t, docs, "DaemonSet") {
		out = append(out, podSpec{"DaemonSet", ds.Name, ds.Spec.Template.Spec})
	}
	return out
}

// containers returns the init containers as well as the regular ones. An init
// container runs with its own securityContext and can hold privileges the
// containers beside it do not, so a check that walks only Spec.Containers is a
// check with a hole in it. The chart renders none today; this is what makes the
// first one that appears subject to the same assertions as everything else.
func (p podSpec) containers() []corev1.Container {
	return append(append([]corev1.Container{}, p.spec.InitContainers...), p.spec.Containers...)
}

func allContainers(t *testing.T, docs []string) []corev1.Container {
	t.Helper()
	var out []corev1.Container
	for _, p := range allPodSpecs(t, docs) {
		out = append(out, p.containers()...)
	}
	return out
}

func sameCapabilities(got, want []corev1.Capability) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[corev1.Capability]bool{}
	for _, c := range got {
		seen[c] = true
	}
	for _, c := range want {
		if !seen[c] {
			return false
		}
	}
	return true
}

// The chart's version floor is a support statement, and README already makes
// one. Two places stating the same promise drift — this repository has watched
// it happen (ADR 0022) — so the chart is checked against the document a
// customer reads.
func TestTheChartsVersionFloorIsTheBaselineWePromise(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("reading README: %v", err)
	}
	stated := regexp.MustCompile(`Baseline: Kubernetes (\d+\.\d+)\+`).FindSubmatch(readme)
	if stated == nil {
		t.Fatal("README no longer states a baseline Kubernetes version in the form this test reads")
	}

	chartYAML, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		t.Fatalf("reading Chart.yaml: %v", err)
	}
	declared := regexp.MustCompile(`kubeVersion:\s*">=(\d+\.\d+)`).FindSubmatch(chartYAML)
	if declared == nil {
		t.Fatal("the chart declares no kubeVersion floor")
	}

	if string(declared[1]) != string(stated[1]) {
		t.Errorf("the chart installs on Kubernetes >=%s but README promises %s+; a chart that installs where the product does not promise to work is a support statement nobody made",
			declared[1], stated[1])
	}
}

// Root is one role's exception, not the product's default (ADR 0037). The
// controller needs no privilege and takes the image's non-root uid; the node
// overrides back to root because a capability granted to a non-root process does
// not survive execve without the ambient set, which Kubernetes does not populate.
//
// Both halves are asserted: the exception must stay explicit, so a node silently
// inheriting root from a changed image is a failure rather than a continuation.
func TestRootIsOneRolesExceptionAndTheControllerDoesNotTakeIt(t *testing.T) {
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			docs := render(t, values)

			pod := only(t, decode[appsv1.Deployment](t, docs, "Deployment"), "Deployment").Spec.Template.Spec
			if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
				t.Error("the controller does not assert runAsNonRoot; it would inherit whatever the image happens to be")
			}
			if pod.SecurityContext != nil && pod.SecurityContext.RunAsUser != nil {
				t.Errorf("the controller pins runAsUser=%d; the uid belongs to the image, and repeating it here is the same fact in two places",
					*pod.SecurityContext.RunAsUser)
			}
			// And no fsGroup: kubelet creates an emptyDir world-writable, so
			// the non-root uid writes the spool without one. A setting that
			// does nothing is a belief nobody checked (ADR 0037).
			if pod.SecurityContext != nil && pod.SecurityContext.FSGroup != nil {
				t.Errorf("the controller sets fsGroup=%d; an emptyDir needs none, and the spool is always an emptyDir",
					*pod.SecurityContext.FSGroup)
			}

			for _, ds := range decode[appsv1.DaemonSet](t, docs, "DaemonSet") {
				node := ds.Spec.Template.Spec.SecurityContext
				if node == nil || node.RunAsUser == nil || *node.RunAsUser != 0 {
					t.Error("the node does not ask for root explicitly; it must override the image's default rather than depend on it")
				}
				if node.RunAsNonRoot == nil || *node.RunAsNonRoot {
					t.Error("the node does not declare that it runs as root")
				}
			}
		})
	}
}

// The agent is a guest of a cluster it did not come with, and ADR 0068 turns
// four omissions into decisions. Two of them are values with defaults, and the
// defaults are the decision: 30s is the controller's shutdown pass plus room,
// 10s is a DaemonSet that has nothing to lose bounding a drain it delays.
func TestThePodsSayWhatTheyCostTheClusterToRemove(t *testing.T) {
	want := map[string]int64{"Deployment": 30, "DaemonSet": 10}
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			for _, pod := range allPodSpecs(t, render(t, values)) {
				grace := pod.spec.TerminationGracePeriodSeconds
				if grace == nil {
					t.Errorf("%s sets no terminationGracePeriodSeconds; the shutdown pass would run inside a number nobody chose (ADR 0068 §4)", pod)
					continue
				}
				if *grace != want[pod.kind] {
					t.Errorf("%s terminationGracePeriodSeconds = %d, want %d", pod, *grace, want[pod.kind])
				}
				// Empty by default and by decision: the chart creates no
				// PriorityClass, so a name here would name nothing.
				if pod.spec.PriorityClassName != "" {
					t.Errorf("%s defaults priorityClassName to %q; the chart ships no PriorityClass for it to name (ADR 0068 §1)",
						pod, pod.spec.PriorityClassName)
				}
			}
		})
	}
}

// And the name an operator does set reaches both pods. The whole point is that
// the agent can be told to yield, so a value that renders on one role only is a
// promise kept on one role only.
func TestThePriorityClassNameReachesBothRoles(t *testing.T) {
	values := ebpfValues()
	values["controller"] = map[string]any{"priorityClassName": "rebuildstack-yields"}
	values["node"] = map[string]any{"priorityClassName": "rebuildstack-yields"}
	for _, pod := range allPodSpecs(t, render(t, values)) {
		if pod.spec.PriorityClassName != "rebuildstack-yields" {
			t.Errorf("%s priorityClassName = %q, want the configured class", pod, pod.spec.PriorityClassName)
		}
	}
}

// A class admission will not accept is refused at render time. `system-` names
// are reserved for kube-system, so on any other release namespace the pods are
// rejected outright — a DaemonSet that creates no pods and does not say why.
func TestAPriorityClassTheClusterWillNotAdmitIsRefused(t *testing.T) {
	for _, role := range []string{"controller", "node"} {
		values := map[string]any{
			"profile": "inventory",
			role:      map[string]any{"priorityClassName": "system-node-critical"},
		}
		if _, err := chartrender.Manifests(chartDir, chartrender.Options{Namespace: "observability", Values: values}); err == nil {
			t.Errorf("the chart rendered %s.priorityClassName=system-node-critical outside kube-system", role)
		}
		// Installed where admission does accept it, it renders: the chart must
		// refuse what the cluster refuses, and nothing else.
		if _, err := chartrender.Manifests(chartDir, chartrender.Options{Namespace: "kube-system", Values: values}); err != nil {
			t.Errorf("the chart refuses a system- class in kube-system, where admission accepts it: %v", err)
		}
	}
}

// The floor under the controller's grace period, which exists because what it
// prevents is silent: below the receiver's own 10s shutdown budget the SIGKILL
// lands before the flush pass and the journals go with no line saying so.
func TestAGracePeriodTooShortForTheShutdownPassIsRefused(t *testing.T) {
	cases := []struct {
		values  map[string]any
		refused bool
	}{
		{map[string]any{"controller": map[string]any{"terminationGracePeriodSeconds": 14}}, true},
		{map[string]any{"controller": map[string]any{"terminationGracePeriodSeconds": 15}}, false},
		{map[string]any{"controller": map[string]any{"terminationGracePeriodSeconds": 120}}, false},
		{map[string]any{"node": map[string]any{"terminationGracePeriodSeconds": 0}}, true},
		{map[string]any{"node": map[string]any{"terminationGracePeriodSeconds": 1}}, false},
	}
	for _, tc := range cases {
		tc.values["profile"] = "inventory"
		_, err := chartrender.Manifests(chartDir, chartrender.Options{Values: tc.values})
		if tc.refused && err == nil {
			t.Errorf("the chart accepted %v", tc.values)
		}
		if !tc.refused && err != nil {
			t.Errorf("the chart refused %v: %v", tc.values, err)
		}
	}
}

// The memory limit has to reach the process, because the Go runtime does not
// read it: it reads the CPU limit and has no memory equivalent (ADR 0068 §5).
// The container name matters — a resourceFieldRef naming the wrong container
// renders, installs, and reports somebody else's limit.
func TestTheMemoryLimitReachesTheAgent(t *testing.T) {
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			for _, pod := range allPodSpecs(t, render(t, values)) {
				for _, container := range pod.containers() {
					ref := resourceFieldRef(container, "MEMORY_LIMIT_BYTES")
					if ref == nil {
						t.Errorf("%s: container %q is not told its memory limit; the Go runtime does not read one (ADR 0068 §5)", pod, container.Name)
						continue
					}
					if ref.Resource != "limits.memory" {
						t.Errorf("%s: MEMORY_LIMIT_BYTES reads %q, want limits.memory", pod, ref.Resource)
					}
					if ref.ContainerName != container.Name {
						t.Errorf("%s: MEMORY_LIMIT_BYTES on %q reads container %q's limit",
							pod, container.Name, ref.ContainerName)
					}
					if ref.Divisor.Value() > 1 {
						t.Errorf("%s: MEMORY_LIMIT_BYTES has divisor %s; anything but bytes makes the value a count of something the agent does not parse",
							pod, ref.Divisor.String())
					}
				}
			}
		})
	}
}

// And it is absent where there is no limit to read. The downward API substitutes
// the node's allocatable memory for a limit that is not set, so rendering the
// variable unconditionally would size the agent to the node — quietly, and worst
// on the largest node in the cluster.
func TestNoMemoryLimitMeansNoVariableRatherThanTheNodesMemory(t *testing.T) {
	// The two roles lose the limit differently, and neither is "write your own
	// requests". The controller's resources deep-merge with the chart's, so a
	// partial override keeps the default limit and only an explicit null
	// removes it; the node's replace the per-profile defaults whole, so
	// resources without a limit is a pod without one.
	cases := map[string]map[string]any{
		"controller": {"profile": "inventory", "controller": map[string]any{
			"resources": map[string]any{"limits": map[string]any{"memory": nil}},
		}},
		"node": {"profile": "inventory", "node": map[string]any{
			"resources": map[string]any{"requests": map[string]any{"cpu": "20m", "memory": "32Mi"}},
		}},
	}
	for role, values := range cases {
		t.Run(role, func(t *testing.T) {
			for _, pod := range allPodSpecs(t, render(t, values)) {
				for _, container := range pod.containers() {
					if container.Name != role {
						continue
					}
					if ref := resourceFieldRef(container, "MEMORY_LIMIT_BYTES"); ref != nil {
						t.Errorf("%s: container %q has no memory limit but is told one; the downward API would report the node's allocatable memory",
							pod, container.Name)
					}
				}
			}
		})
	}
}

// resourceFieldRef returns the downward-API resource reference behind the named
// environment variable, or nil if it is absent or sourced some other way.
func resourceFieldRef(container corev1.Container, name string) *corev1.ResourceFieldSelector {
	for _, env := range container.Env {
		if env.Name != name || env.ValueFrom == nil {
			continue
		}
		return env.ValueFrom.ResourceFieldRef
	}
	return nil
}
