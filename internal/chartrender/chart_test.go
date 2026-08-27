package chartrender_test

import (
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

// What this file is for.
//
// The chart is the only installer, so a mistake in it is a mistake in the
// product: a missing grant, a capability nobody meant to add, a config key the
// agent will reject at startup. None of that is visible in a diff of Go code,
// and the e2e proves only the profile it happens to install.
//
// So the promises docs/security.md makes about an installation are asserted
// here, against the rendered manifests, for every profile.

// chartDir is the chart's path from this package.
const chartDir = "../../" + chartrender.Dir

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
		"":                  {"pods", "namespaces", "nodes", "limitranges", "resourcequotas", "persistentvolumeclaims"},
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
// a controller-only install opens no port and has nothing listening.
func TestMetricsOnlyInstallsNoNodeAndOpensNoPort(t *testing.T) {
	docs := render(t, map[string]any{"profile": "metrics-only"})
	if ds := decode[appsv1.DaemonSet](t, docs, "DaemonSet"); len(ds) != 0 {
		t.Error("metrics-only rendered a DaemonSet")
	}
	if svc := decode[corev1.Service](t, docs, "Service"); len(svc) != 0 {
		t.Error("metrics-only rendered a Service; nothing sends it reports")
	}
	deploy := only(t, decode[appsv1.Deployment](t, docs, "Deployment"), "Deployment")
	if ports := deploy.Spec.Template.Spec.Containers[0].Ports; len(ports) != 0 {
		t.Errorf("metrics-only opens ports %v", ports)
	}
	if sa := decode[corev1.ServiceAccount](t, docs, "ServiceAccount"); len(sa) != 1 {
		t.Errorf("metrics-only rendered %d ServiceAccounts, want only the controller's", len(sa))
	}
	if np := decode[networkingv1.NetworkPolicy](t, docs, "NetworkPolicy"); len(np) != 0 {
		t.Error("metrics-only rendered a NetworkPolicy; there is no port to restrict")
	}
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

// TestTheReceiverIsRestrictedToTheNodeDaemonSet is the policy ADR 0010, 0011
// and 0015 described as shipped for months while it did not exist (ADR 0039).
// It is asserted rather than reviewed for exactly that reason: the failure mode
// was not that someone wrote the wrong policy, it was that everyone believed
// there was one.
//
// What the assertions insist on, and why each would be a silent hole:
//   - `policyTypes: [Ingress]` with a non-empty rule — an absent policyTypes on
//     an object with no ingress rules allows everything;
//   - exactly one `from` peer, and it a podSelector — a second peer, or an
//     empty `from: [{}]`, re-opens the port to the cluster;
//   - the peer selects the node component of this release — a selector that
//     matches nothing denies the node itself, and a broader one admits
//     strangers;
//   - the port is the receiver's — a policy on the wrong port is a policy on
//     nothing.
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

			if len(np.Spec.Ingress) != 1 {
				t.Fatalf("ingress has %d rules, want exactly 1 — every extra rule is another way in", len(np.Spec.Ingress))
			}
			rule := np.Spec.Ingress[0]
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
			port := deploy.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort
			if rule.Ports[0].Port == nil || rule.Ports[0].Port.String() != strconv.Itoa(int(port)) {
				t.Errorf("ingress port = %v, want the receiver's %d", rule.Ports[0].Port, port)
			}
		})
	}
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

// The rule the chart adds that no manifest ever enforced. The sample shipped
// before the chart turned the profiler on with an empty allow-list, which
// admits no frame: profiling would run and produce nothing, forever, silently
// (ADR 0011 §4, and the shape of trap ADR 0025 found in `eligibleNamespaces`).
func TestTheProfilerWillNotInstallWithAnEmptyAllowList(t *testing.T) {
	_, err := chartrender.Manifests(chartDir, chartrender.Options{
		Values: map[string]any{"profile": "ebpf"},
	})
	if err == nil {
		t.Fatal("the chart rendered the profiler with an empty allow-list")
	}
	if !strings.Contains(err.Error(), "allowedModulePrefixes") {
		t.Errorf("the refusal does not name the value to set: %v", err)
	}
}

// A profile the agent cannot honour must fail rather than look honoured. The
// `pprof` puller does not exist, and docs/security.md says so.
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

// Root is one role's exception, not the product's default (ADR 0037).
//
// The controller reads the API and writes a spool; it needs no privilege, so it
// takes the image's non-root uid and asserts it. The node overrides back to root
// because reading another container's /proc/<pid>/exe cannot be bought with
// CAP_SYS_PTRACE alone — a capability granted to a non-root process does not
// survive execve without the ambient set, and Kubernetes does not populate it.
//
// Both halves are asserted, and the second matters as much as the first: the
// exception must stay explicit, so that a node silently inheriting root from a
// changed image is a failure rather than a continuation.
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
