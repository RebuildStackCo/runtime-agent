package chartrender_test

import (
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// These tests assert absences, which chart_test.go cannot: it enumerates what
// the chart must contain, and a presence test is indifferent to an addition
// standing next to it. Five chart mutations that break plain promises in
// docs/security.md — `secrets` in a read-only rule, hostPID on the controller,
// a capability added beside `drop: [ALL]`, a privileged init container, a
// writable hostPath — each passed the whole suite green (ADR 0044).
//
// So each test here names the complete permitted set and fails on anything else.

// The controller's grant is a closed list, not a floor: one word added to an
// existing rule keeps every verb read-only, keeps every required grant in place,
// and quietly breaks docs/security.md §9. The permitted set is spelled out here
// rather than derived from the chart, because a test that reads its expectation
// from the thing it tests asserts nothing.
func TestTheClusterRoleNamesNothingBeyondThisList(t *testing.T) {
	permitted := map[string][]string{
		"":                  {"pods", "namespaces", "nodes", "nodes/proxy", "services", "limitranges", "resourcequotas", "persistentvolumeclaims"},
		"apps":              {"replicasets", "deployments", "statefulsets", "daemonsets"},
		"batch":             {"jobs", "cronjobs"},
		"discovery.k8s.io":  {"endpointslices"},
		"policy":            {"poddisruptionbudgets"},
		"autoscaling":       {"horizontalpodautoscalers"},
		"scheduling.k8s.io": {"priorityclasses"},
		"storage.k8s.io":    {"storageclasses"},
	}
	want := map[string]bool{}
	for group, resources := range permitted {
		for _, resource := range resources {
			want[group+"/"+resource] = true
		}
	}

	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			role := only(t, decode[rbacv1.ClusterRole](t, render(t, values), "ClusterRole"), "ClusterRole")
			for _, rule := range role.Rules {
				for _, group := range rule.APIGroups {
					for _, resource := range rule.Resources {
						if !want[group+"/"+resource] {
							t.Errorf("the ClusterRole names %q in apiGroup %q, which is not on the permitted list; "+
								"the agent opens no informer on it and docs/security.md §9 says it is not requested",
								resource, group)
						}
					}
				}
			}
		})
	}
}

// The two non-resource GETs are OIDC discovery and JWKS, and they exist to
// validate node tokens locally instead of calling TokenReview (ADR 0010). A
// third URL would be a capability nothing in the agent uses, and the profile
// with no node must ask for neither.
func TestTheOnlyNonResourceURLsAreTheOnesNodeTokensNeed(t *testing.T) {
	oidc := []string{"/.well-known/openid-configuration", "/openid/v1/jwks"}
	cases := map[string][]string{
		"metrics-only": nil, // no node, no token to validate, no reason to ask
		"inventory":    oidc,
		"ebpf":         oidc,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			role := only(t, decode[rbacv1.ClusterRole](t, render(t, profiles()[name]), "ClusterRole"), "ClusterRole")
			var got []string
			for _, rule := range role.Rules {
				got = append(got, rule.NonResourceURLs...)
			}
			sort.Strings(got)
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("nonResourceURLs = %v, want exactly %v", got, want)
			}
		})
	}
}

// docs/security.md §2 draws the host boundary between the two roles. hostPID is
// what the scanner needs to see other containers' /proc (ADR 0009), so it is
// asserted both ways — absent from the controller, present on the node, since
// losing it there makes the scanner collect nothing and say nothing about why.
//
// hostNetwork and hostIPC are on no pod: hostNetwork would put the receiver's
// port on the node's address, where the NetworkPolicy does not reach.
func TestOnlyTheNodeSharesTheHostsNamespaces(t *testing.T) {
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			for _, pod := range allPodSpecs(t, render(t, values)) {
				if pod.spec.HostNetwork {
					t.Errorf("%s sets hostNetwork; no component of this agent uses the node's network namespace", pod)
				}
				if pod.spec.HostIPC {
					t.Errorf("%s sets hostIPC; no component of this agent uses the node's IPC namespace", pod)
				}
				switch pod.kind {
				case "Deployment":
					if pod.spec.HostPID {
						t.Errorf("%s sets hostPID; the controller reads the API server, not the node (docs/security.md §2)", pod)
					}
				case "DaemonSet":
					if !pod.spec.HostPID {
						t.Errorf("%s does not set hostPID; without it the scanner sees only its own /proc and collects nothing (ADR 0009)", pod)
					}
				}
			}
		})
	}
}

// docs/security.md §7 promises the added capabilities belong to the node role
// alone; chart_test.go pins the node's set, and every other container must add
// nothing at all.
//
// `drop: [ALL]` does not cover this: the runtime applies the drop and then the
// add, so the add wins. Measured on 1.36.1, a container with `drop: [ALL]` and
// `add: [SYS_ADMIN, NET_ADMIN]` has CapEff 0000000000201000 — bits 21 and 12,
// exactly the two added, in a securityContext that reads as careful.
func TestOnlyTheNodeContainerAddsACapability(t *testing.T) {
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			for _, pod := range allPodSpecs(t, render(t, values)) {
				if pod.kind == "DaemonSet" {
					continue // its exact set is asserted in chart_test.go
				}
				for _, container := range pod.containers() {
					sc := container.SecurityContext
					if sc == nil || sc.Capabilities == nil {
						continue // the privileged/drop-ALL assertions cover an absent context
					}
					if len(sc.Capabilities.Add) != 0 {
						t.Errorf("%s: container %q adds %v; only the node role adds capabilities (docs/security.md §7)",
							pod, container.Name, sc.Capabilities.Add)
					}
				}
			}
		})
	}
}

// The controller touches no part of the node it runs on. TestEveryHostMountIsReadOnly
// bounds how a host path may be mounted; this says the controller may not mount
// one at all, read-only included — §7's blast-radius paragraphs are written
// about the DaemonSet on the understanding that the controller has no reach.
//
// Same for volumes that read the cluster around the pod: config comes from its
// own ConfigMap, and a projected token belongs to the node (ADR 0010).
func TestTheControllerReachesNothingOutsideItsOwnPod(t *testing.T) {
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			for _, pod := range allPodSpecs(t, render(t, values)) {
				if pod.kind != "Deployment" {
					continue
				}
				for _, volume := range pod.spec.Volumes {
					switch {
					case volume.HostPath != nil:
						t.Errorf("%s mounts host path %q; the controller reads the API server, not a node (docs/security.md §7)",
							pod, volume.HostPath.Path)
					case volume.Secret != nil:
						t.Errorf("%s mounts Secret %q; the agent reads no Secret (docs/security.md §9)",
							pod, volume.Secret.SecretName)
					case volume.Projected != nil:
						t.Errorf("%s mounts a projected volume; the token side of the node channel is the node's (ADR 0010)", pod)
					}
				}
			}
		})
	}
}

// A guard on the guards. Every assertion above is written per pod spec, so it is
// only as complete as allPodSpecs, and allPodSpecs knows the two workload kinds
// the chart renders today. If a third appears — a Job, a CronJob, a second
// Deployment — it would be silently exempt from all of them.
//
// A rendered kind is either a workload this file must walk or it is not one, and
// that is a decision to make deliberately rather than discover later.
func TestNoWorkloadKindEscapesThePodSpecWalk(t *testing.T) {
	walked := map[string]bool{"Deployment": true, "DaemonSet": true}
	notWorkloads := map[string]bool{
		"ServiceAccount": true, "ClusterRole": true, "ClusterRoleBinding": true,
		"ConfigMap": true, "Service": true, "NetworkPolicy": true,
	}
	for name, values := range profiles() {
		t.Run(name, func(t *testing.T) {
			for _, doc := range render(t, values) {
				kind := kindOf(t, doc)
				if !walked[kind] && !notWorkloads[kind] {
					t.Errorf("the chart renders a %s, which no guardrail in this file walks; "+
						"add it to allPodSpecs if it carries a pod, or to this test's list if it does not", kind)
				}
			}
		})
	}
}

func kindOf(t *testing.T, doc string) string {
	t.Helper()
	var meta metav1.TypeMeta
	if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
		t.Fatalf("decoding TypeMeta: %v\n%s", err, doc)
	}
	if meta.Kind == "" {
		t.Fatalf("document has no kind:\n%s", doc)
	}
	return meta.Kind
}
