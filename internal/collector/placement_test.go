package collector

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// secretVolumeName is the name of the Secret volume in the leak fixture. It is a
// constant so the string appears once: the test asserts the payload never
// carries it, and a literal repeated in both places invites one of them being
// edited alone.
const secretVolumeName = "prod-db-credentials" //nolint:gosec // a fixture Secret name, and it reads like one on purpose: the test asserts it never reaches the payload

func TestPlacementReducesEveryConstrainingField(t *testing.T) {
	spec := &corev1.PodSpec{
		NodeSelector: map[string]string{"node.kubernetes.io/instance-type": "m6i.4xlarge"},
		Affinity: &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "topology.kubernetes.io/zone",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"us-east-1a"},
						}},
					}},
				},
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
					Weight: 50,
					Preference: corev1.NodeSelectorTerm{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "workload",
							Operator: corev1.NodeSelectorOpExists,
						}},
					},
				}},
			},
			PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					TopologyKey: "kubernetes.io/hostname",
				}},
			},
		},
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
			TopologyKey:       "topology.kubernetes.io/zone",
			MaxSkew:           1,
			WhenUnsatisfiable: corev1.DoNotSchedule,
			MinDomains:        ptr.To(int32(3)),
		}},
		Tolerations: []corev1.Toleration{{
			Key: "dedicated", Operator: corev1.TolerationOpEqual,
			Value: "gpu", Effect: corev1.TaintEffectNoSchedule,
		}},
		PriorityClassName:             "high",
		TerminationGracePeriodSeconds: ptr.To(int64(300)),
		HostNetwork:                   true,
		SchedulerName:                 "karpenter",
	}

	got, drops := reducePlacement(spec)
	if !drops.empty() {
		t.Fatalf("nothing should be dropped from an ordinary spec, got %+v", drops)
	}

	if got.NodeSelector["node.kubernetes.io/instance-type"] != "m6i.4xlarge" {
		t.Errorf("node selector = %v", got.NodeSelector)
	}
	if len(got.NodeAffinity) != 2 {
		t.Fatalf("node affinity terms = %d, want 2 (one required, one preferred)", len(got.NodeAffinity))
	}
	req := got.NodeAffinity[0]
	if !req.Required || req.Key != "topology.kubernetes.io/zone" || req.Operator != "In" {
		t.Errorf("required node affinity = %+v", req)
	}
	if len(req.Values) != 1 || req.Values[0] != "us-east-1a" {
		t.Errorf("required node affinity values = %v", req.Values)
	}
	// A preference and a requirement are different facts: only the second can
	// leave a pod unschedulable, and flattening them together would erase the
	// distinction the whole field exists for.
	pref := got.NodeAffinity[1]
	if pref.Required || pref.Weight != 50 {
		t.Errorf("preferred node affinity = %+v, want not required with weight 50", pref)
	}

	if len(got.PodAntiAffinity) != 1 || got.PodAntiAffinity[0].TopologyKey != "kubernetes.io/hostname" ||
		!got.PodAntiAffinity[0].Required {
		t.Errorf("pod anti-affinity = %+v", got.PodAntiAffinity)
	}
	if len(got.TopologySpread) != 1 {
		t.Fatalf("topology spread = %+v", got.TopologySpread)
	}
	spread := got.TopologySpread[0]
	if spread.WhenUnsatisfiable != "DoNotSchedule" || spread.MaxSkew != 1 ||
		spread.MinDomains == nil || *spread.MinDomains != 3 {
		t.Errorf("topology spread = %+v", spread)
	}
	if len(got.Tolerations) != 1 || got.Tolerations[0].Value != "gpu" {
		t.Errorf("tolerations = %+v", got.Tolerations)
	}
	if got.PriorityClass != "high" || got.SchedulerName != "karpenter" || !got.HostNetwork {
		t.Errorf("scalar fields = %+v", got)
	}
	if got.TerminationGraceSeconds == nil || *got.TerminationGraceSeconds != 300 {
		t.Errorf("termination grace = %v", got.TerminationGraceSeconds)
	}
}

// An unconstrained pod must contribute nothing. Every field is omitempty, so the
// block disappears from the payload rather than repeating nine empty values on
// every record of every container of every workload.
func TestUnconstrainedPodProducesNoPlacementBytes(t *testing.T) {
	spec := &corev1.PodSpec{
		SchedulerName:                 defaultSchedulerName,
		TerminationGracePeriodSeconds: ptr.To(defaultTerminationGrace),
		Tolerations: []corev1.Toleration{
			{
				Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists,
				Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(defaultTolerationSeconds),
			},
			{
				Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists,
				Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(defaultTolerationSeconds),
			},
		},
	}
	got, drops := reducePlacement(spec)
	if !drops.empty() {
		t.Errorf("cluster defaults are not drops, got %+v", drops)
	}

	encoded, err := json.Marshal(struct {
		Placement Placement `json:"placement,omitzero"`
	}{got})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{}" {
		t.Errorf("unconstrained pod encoded as %s, want {}", encoded)
	}
}

// The two tolerations the cluster puts on every pod say nothing; a pod that
// tuned them says how fast it leaves a failing node, which is a real fact.
func TestTunedNodeConditionTolerationSurvives(t *testing.T) {
	spec := &corev1.PodSpec{Tolerations: []corev1.Toleration{{
		Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists,
		Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(0)),
	}}}
	got, _ := reducePlacement(spec)
	if len(got.Tolerations) != 1 {
		t.Fatalf("tolerations = %+v, want the tuned one kept", got.Tolerations)
	}
	if got.Tolerations[0].Seconds == nil || *got.Tolerations[0].Seconds != 0 {
		t.Errorf("toleration seconds = %v, want 0", got.Tolerations[0].Seconds)
	}
}

func TestOverlongValueDropsItsTermAndIsCounted(t *testing.T) {
	long := strings.Repeat("x", maxPlacementValue+1)
	spec := &corev1.PodSpec{
		NodeSelector: map[string]string{"kept": "value", "overlong": long},
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: long, Operator: corev1.NodeSelectorOpExists,
					}},
				}},
			},
		}},
	}
	got, drops := reducePlacement(spec)
	if len(got.NodeSelector) != 1 || got.NodeSelector["kept"] != "value" {
		t.Errorf("node selector = %v, want only the short entry", got.NodeSelector)
	}
	if len(got.NodeAffinity) != 0 {
		t.Errorf("node affinity = %+v, want the overlong term dropped", got.NodeAffinity)
	}
	if drops.Values != 2 {
		t.Errorf("dropped values = %d, want 2", drops.Values)
	}
}

// A value list past the bound is dropped whole rather than cut, because a
// partial list reads as a complete one. The key and operator survive, which is
// the half that says what the workload is pinned on.
func TestOverlongValueListDropsValuesAndKeepsTheKey(t *testing.T) {
	values := make([]string, maxPlacementValues+1)
	for i := range values {
		values[i] = "zone"
	}
	spec := &corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "topology.kubernetes.io/zone", Operator: corev1.NodeSelectorOpIn, Values: values,
				}},
			}},
		},
	}}}
	got, drops := reducePlacement(spec)
	if len(got.NodeAffinity) != 1 {
		t.Fatalf("node affinity = %+v, want the term kept", got.NodeAffinity)
	}
	if got.NodeAffinity[0].Key != "topology.kubernetes.io/zone" || got.NodeAffinity[0].Values != nil {
		t.Errorf("term = %+v, want the key with no values", got.NodeAffinity[0])
	}
	if drops.Values != 1 {
		t.Errorf("dropped values = %d, want 1", drops.Values)
	}
}

func TestTooManyTermsAreCappedAndCounted(t *testing.T) {
	var tolerations []corev1.Toleration
	for i := 0; i < maxPlacementTerms+5; i++ {
		tolerations = append(tolerations, corev1.Toleration{
			Key: "taint", Operator: corev1.TolerationOpExists,
		})
	}
	got, drops := reducePlacement(&corev1.PodSpec{Tolerations: tolerations})
	if len(got.Tolerations) != maxPlacementTerms {
		t.Errorf("tolerations kept = %d, want %d", len(got.Tolerations), maxPlacementTerms)
	}
	if drops.Terms != 5 {
		t.Errorf("dropped terms = %d, want 5", drops.Terms)
	}
}

// The placement fields sit in the same PodSpec as env, args and command, which
// are never read (CLAUDE.md invariant 4). This checks the encoded bytes rather
// than the shape of the struct, so a field added later that carries part of the
// spec through fails here rather than in a customer's cluster.
func TestPlacementCarriesNothingFromTheContainers(t *testing.T) {
	spec := &corev1.PodSpec{
		NodeSelector: map[string]string{"pool": "gpu"},
		Containers: []corev1.Container{{
			Name:    "app",
			Image:   "example.com/app:1",
			Command: []string{"/bin/app", "--token=secret"},
			Args:    []string{"--password", "hunter2"},
			Env: []corev1.EnvVar{
				{Name: "DB_PASSWORD", Value: "hunter2"},
				{Name: "API_TOKEN", Value: "sk-live-4242"},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		}},
		Volumes: []corev1.Volume{{
			Name: "creds",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: secretVolumeName},
			},
		}},
	}
	got, _ := reducePlacement(spec)
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"hunter2", "DB_PASSWORD", "API_TOKEN", "sk-live-4242",
		"--token=secret", "--password", "/bin/app",
		secretVolumeName, "creds",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("placement payload contains %q:\n%s", forbidden, encoded)
		}
	}
}

// The counters must reach the coverage report through the watcher, not just
// exist inside the reduction.
func TestPlacementDropsReachTheWatcherCounters(t *testing.T) {
	w := &PodWatcher{}
	long := strings.Repeat("x", maxPlacementValue+1)
	w.describe(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1"},
		Spec:       corev1.PodSpec{NodeSelector: map[string]string{"pool": long}},
	})
	if got := w.PlacementDrops(); got.Values != 1 {
		t.Errorf("placement drops = %+v, want one dropped value", got)
	}
}
