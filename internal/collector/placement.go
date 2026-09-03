package collector

import (
	"github.com/RebuildStackCo/runtime-agent/internal/model"
	corev1 "k8s.io/api/core/v1"
)

// maxPlacementValue bounds one operator-written string kept from a pod's
// placement. Label keys, label values, taint keys and topology keys are written
// by whoever wrote the manifest, so this is the second place after build
// settings where free-form strings enter the agent (ADR 0019). A value over the
// bound means the string is not what this reduction assumes, and the term
// carrying it is dropped rather than truncated: a prefix of an unexpected string
// is still an unexpected string.
const maxPlacementValue = 128

// maxPlacementTerms bounds how many terms of one kind are kept from one pod.
// Real manifests hold a handful; a pod past this bound is not describing
// placement the way this reduction assumes.
const maxPlacementTerms = 32

// maxPlacementValues bounds the values of one match expression. Past it the
// values are dropped and the term is kept: the key is the half that says what
// the workload is pinned on, and a partial value list would read as a complete
// one.
const maxPlacementValues = 16

// defaultTerminationGrace is Kubernetes' own default. A pod that carries it says
// nothing, and repeating it on every record would be noise; a pod that deviates
// is stating how long draining it takes, which is the cost of consolidation.
const defaultTerminationGrace int64 = 30

// defaultSchedulerName is the scheduler every pod gets unless someone chose
// otherwise. Only the choice is a fact.
const defaultSchedulerName = "default-scheduler"

// defaultTolerationSeconds is what the DefaultTolerationSeconds admission plugin
// puts on the two node-condition tolerations it adds to every pod in the
// cluster. See tolerationIsClusterDefault.
const defaultTolerationSeconds int64 = 300

// placementDrops counts what the reduction refused to carry. It is aggregate:
// what was dropped is counted, never named (CLAUDE.md invariant 6), and it
// reaches the coverage report so a cluster whose manifests do not fit these
// bounds is visible rather than quietly under-reported.
type placementDrops struct {
	// Values counts terms dropped because a string exceeded maxPlacementValue,
	// and value lists dropped because there were more than maxPlacementValues.
	Values int
	// Terms counts terms dropped because a list exceeded maxPlacementTerms.
	Terms int
}

func (d *placementDrops) empty() bool { return d.Values == 0 && d.Terms == 0 }

// reducePlacement reads the nine placement fields of a pod spec and returns the
// reduced view plus what it refused to carry.
//
// A reduction, not a faithful copy: node affinity's terms are OR'd across and
// AND'd within, and flattening loses that. What survives answers "what is this
// pinned on, and how hard"; replaying the scheduler's decision is not offered.
// `env`, `args`, `command` and `volumes` sit in the same PodSpec and are never
// read (CLAUDE.md invariant 4).
func reducePlacement(spec *corev1.PodSpec) (model.Placement, placementDrops) {
	var p model.Placement
	var drops placementDrops

	if len(spec.NodeSelector) > 0 {
		sel := make(map[string]string, len(spec.NodeSelector))
		for k, v := range spec.NodeSelector {
			if !fits(k) || !fits(v) {
				drops.Values++
				continue
			}
			sel[k] = v
		}
		if len(sel) > 0 {
			p.NodeSelector = sel
		}
	}

	if a := spec.Affinity; a != nil {
		p.NodeAffinity = reduceNodeAffinity(a.NodeAffinity, &drops)
		p.PodAffinity = reducePodAffinity(a.PodAffinity, &drops)
		p.PodAntiAffinity = reducePodAntiAffinity(a.PodAntiAffinity, &drops)
	}

	p.TopologySpread = reduceSpread(spec.TopologySpreadConstraints, &drops)
	p.Tolerations = reduceTolerations(spec.Tolerations, &drops)

	if fits(spec.PriorityClassName) {
		p.PriorityClass = spec.PriorityClassName
	} else if spec.PriorityClassName != "" {
		drops.Values++
	}

	if g := spec.TerminationGracePeriodSeconds; g != nil && *g != defaultTerminationGrace {
		grace := *g
		p.TerminationGraceSeconds = &grace
	}

	p.HostNetwork = spec.HostNetwork

	if spec.SchedulerName != "" && spec.SchedulerName != defaultSchedulerName {
		if fits(spec.SchedulerName) {
			p.SchedulerName = spec.SchedulerName
		} else {
			drops.Values++
		}
	}

	return p, drops
}

func reduceNodeAffinity(na *corev1.NodeAffinity, drops *placementDrops) []model.NodeAffinityTerm {
	if na == nil {
		return nil
	}
	var out []model.NodeAffinityTerm
	if req := na.RequiredDuringSchedulingIgnoredDuringExecution; req != nil {
		for _, term := range req.NodeSelectorTerms {
			out = appendSelectorTerm(out, term, true, 0, drops)
		}
	}
	for _, pref := range na.PreferredDuringSchedulingIgnoredDuringExecution {
		out = appendSelectorTerm(out, pref.Preference, false, pref.Weight, drops)
	}
	return capTerms(out, drops)
}

// appendSelectorTerm flattens one NodeSelectorTerm. Match expressions and match
// fields share a shape and are folded into one list; a match field's key is
// `metadata.name`, which is a pin to a single node and belongs beside the label
// constraints rather than in a category of its own.
func appendSelectorTerm(out []model.NodeAffinityTerm, term corev1.NodeSelectorTerm, required bool, weight int32, drops *placementDrops) []model.NodeAffinityTerm {
	add := func(exprs []corev1.NodeSelectorRequirement) {
		for _, e := range exprs {
			if !fits(e.Key) {
				drops.Values++
				continue
			}
			t := model.NodeAffinityTerm{
				Key:      e.Key,
				Operator: string(e.Operator),
				Required: required,
				Weight:   weight,
			}
			t.Values = keepValues(e.Values, drops)
			out = append(out, t)
		}
	}
	add(term.MatchExpressions)
	add(term.MatchFields)
	return out
}

func reducePodAffinity(pa *corev1.PodAffinity, drops *placementDrops) []model.TopologyTerm {
	if pa == nil {
		return nil
	}
	var out []model.TopologyTerm
	out = appendTopologyTerms(out, pa.RequiredDuringSchedulingIgnoredDuringExecution, drops)
	out = appendWeightedTopologyTerms(out, pa.PreferredDuringSchedulingIgnoredDuringExecution, drops)
	return capTerms(out, drops)
}

func reducePodAntiAffinity(pa *corev1.PodAntiAffinity, drops *placementDrops) []model.TopologyTerm {
	if pa == nil {
		return nil
	}
	var out []model.TopologyTerm
	out = appendTopologyTerms(out, pa.RequiredDuringSchedulingIgnoredDuringExecution, drops)
	out = appendWeightedTopologyTerms(out, pa.PreferredDuringSchedulingIgnoredDuringExecution, drops)
	return capTerms(out, drops)
}

func appendTopologyTerms(out []model.TopologyTerm, terms []corev1.PodAffinityTerm, drops *placementDrops) []model.TopologyTerm {
	for _, t := range terms {
		if !fits(t.TopologyKey) {
			drops.Values++
			continue
		}
		out = append(out, model.TopologyTerm{TopologyKey: t.TopologyKey, Required: true})
	}
	return out
}

func appendWeightedTopologyTerms(out []model.TopologyTerm, terms []corev1.WeightedPodAffinityTerm, drops *placementDrops) []model.TopologyTerm {
	for _, t := range terms {
		if !fits(t.PodAffinityTerm.TopologyKey) {
			drops.Values++
			continue
		}
		out = append(out, model.TopologyTerm{
			TopologyKey: t.PodAffinityTerm.TopologyKey,
			Weight:      t.Weight,
		})
	}
	return out
}

func reduceSpread(constraints []corev1.TopologySpreadConstraint, drops *placementDrops) []model.SpreadTerm {
	var out []model.SpreadTerm
	for _, c := range constraints {
		if !fits(c.TopologyKey) {
			drops.Values++
			continue
		}
		term := model.SpreadTerm{
			TopologyKey:       c.TopologyKey,
			MaxSkew:           c.MaxSkew,
			WhenUnsatisfiable: string(c.WhenUnsatisfiable),
		}
		if c.MinDomains != nil {
			domains := *c.MinDomains
			term.MinDomains = &domains
		}
		out = append(out, term)
	}
	return capTerms(out, drops)
}

func reduceTolerations(tolerations []corev1.Toleration, drops *placementDrops) []model.Toleration {
	var out []model.Toleration
	for _, t := range tolerations {
		if tolerationIsClusterDefault(t) {
			continue
		}
		if !fits(t.Key) || !fits(t.Value) {
			drops.Values++
			continue
		}
		kept := model.Toleration{
			Key:      t.Key,
			Operator: string(t.Operator),
			Value:    t.Value,
			Effect:   string(t.Effect),
		}
		if t.TolerationSeconds != nil {
			secs := *t.TolerationSeconds
			kept.Seconds = &secs
		}
		out = append(out, kept)
	}
	return capTerms(out, drops)
}

// tolerationIsClusterDefault reports whether a toleration is one the cluster put
// on every pod rather than one someone wrote: DefaultTolerationSeconds adds
// `not-ready` and `unreachable` at 300s to every pod that does not already
// tolerate them, and repeating those on every record states nothing.
//
// The match is exact on the seconds — a pod tolerating `not-ready` for zero or
// for an hour has been tuned, and that deviation is kept.
func tolerationIsClusterDefault(t corev1.Toleration) bool {
	if t.Key != corev1.TaintNodeNotReady && t.Key != corev1.TaintNodeUnreachable {
		return false
	}
	if t.Operator != corev1.TolerationOpExists || t.Effect != corev1.TaintEffectNoExecute {
		return false
	}
	return t.TolerationSeconds != nil && *t.TolerationSeconds == defaultTolerationSeconds
}

// keepValues returns the values of a match expression, or nothing when there are
// more than the bound allows. Dropping the list rather than cutting it keeps the
// payload from showing a partial set that reads as complete; the term's key and
// operator survive either way, which is the part that says what the workload is
// pinned on.
func keepValues(values []string, drops *placementDrops) []string {
	if len(values) == 0 {
		return nil
	}
	if len(values) > maxPlacementValues {
		drops.Values++
		return nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !fits(v) {
			drops.Values++
			return nil
		}
		out = append(out, v)
	}
	return out
}

// capTerms bounds a term list, counting the excess rather than silently
// shortening it.
func capTerms[T any](terms []T, drops *placementDrops) []T {
	if len(terms) <= maxPlacementTerms {
		return terms
	}
	drops.Terms += len(terms) - maxPlacementTerms
	return terms[:maxPlacementTerms]
}

func fits(s string) bool { return len(s) <= maxPlacementValue }
