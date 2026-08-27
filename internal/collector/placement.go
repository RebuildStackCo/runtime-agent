package collector

import (
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

// Placement is what a pod's spec says about where it may run and how much it
// costs to move it — the missing third of the consolidation question, after what
// a workload asks for and what machine it got.
//
// Every field is a reduction of its pod-spec field, not a copy (reducePlacement),
// and nothing here is judged: whether a hostname anti-affinity is deliberate or
// forgotten is a backend rendering (ADR 0004). All fields are omitempty, so a
// pod with no constraints contributes no bytes.
type Placement struct {
	// NodeSelector is spec.nodeSelector verbatim — it is already the flat
	// key/value form the other fields are reduced to.
	NodeSelector map[string]string `json:"node_selector,omitempty"`
	// NodeAffinity flattens spec.affinity.nodeAffinity. Both the required and
	// the preferred terms land here, distinguished by Required, and match
	// expressions and match fields land in the same list because their shape is
	// identical — a match field with key `metadata.name` is a pin to one node.
	NodeAffinity []NodeAffinityTerm `json:"node_affinity,omitempty"`
	// PodAffinity and PodAntiAffinity keep the topology key and nothing else of
	// the term. A required anti-affinity on `kubernetes.io/hostname` is the
	// single strongest statement in a pod spec about packing: it means one
	// replica per node and no amount of spare capacity elsewhere changes that.
	PodAffinity     []TopologyTerm `json:"pod_affinity,omitempty"`
	PodAntiAffinity []TopologyTerm `json:"pod_anti_affinity,omitempty"`
	// TopologySpread is spec.topologySpreadConstraints minus its selector.
	// `DoNotSchedule` across zones forces the workload to keep paying for every
	// zone it spans, whether or not the capacity is needed there.
	TopologySpread []SpreadTerm `json:"topology_spread,omitempty"`
	// Tolerations are what let the pod sit on tainted nodes — which is usually
	// how a cluster fences off its expensive hardware. The two tolerations the
	// cluster adds to every pod are not kept; see tolerationIsClusterDefault.
	Tolerations []Toleration `json:"tolerations,omitempty"`
	// PriorityClass is the name only. The class object holds the numeric value
	// and the preemption policy, and reading it needs RBAC this agent does not
	// have. The name still explains preemptions already reported in
	// `pod_disruptions`.
	PriorityClass string `json:"priority_class,omitempty"`
	// TerminationGraceSeconds is present only when it deviates from the cluster
	// default. Draining a node for consolidation waits this long per pod.
	TerminationGraceSeconds *int64 `json:"termination_grace_seconds,omitempty"`
	// HostNetwork means the pod holds ports on its node, which bounds how many
	// of its kind fit on one machine regardless of CPU and memory.
	HostNetwork bool `json:"host_network,omitempty"`
	// SchedulerName is present only when it is not the default one. A custom
	// scheduler means the placement facts above may not be the whole rule.
	SchedulerName string `json:"scheduler_name,omitempty"`
}

// NodeAffinityTerm is one requirement on node labels. Required separates a hard
// constraint from a weighted preference; the two are not the same fact, because
// only the first can leave a pod unschedulable.
type NodeAffinityTerm struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	// Values is absent when the operator takes none (`Exists`, `DoesNotExist`)
	// and also when there were more than maxPlacementValues of them, in which
	// case the drop is counted. The operator tells the two apart.
	Values   []string `json:"values,omitempty"`
	Required bool     `json:"required,omitempty"`
	// Weight is the preferred term's weight, absent on required ones.
	Weight int32 `json:"weight,omitempty"`
}

// TopologyTerm is one pod affinity or anti-affinity term reduced to its topology
// key. The label selector inside it says which pods the rule is relative to,
// which is a smaller fact than the rule's existence and its granularity, and
// carrying it would mean copying an arbitrarily nested selector into the
// payload.
type TopologyTerm struct {
	TopologyKey string `json:"topology_key"`
	Required    bool   `json:"required,omitempty"`
	Weight      int32  `json:"weight,omitempty"`
}

// SpreadTerm is one topology spread constraint minus its selector.
type SpreadTerm struct {
	TopologyKey       string `json:"topology_key"`
	MaxSkew           int32  `json:"max_skew"`
	WhenUnsatisfiable string `json:"when_unsatisfiable"`
	// MinDomains forces the spread to span at least this many domains even when
	// fewer would satisfy the skew, so it can hold capacity open in a zone that
	// does not need it.
	MinDomains *int32 `json:"min_domains,omitempty"`
}

// Toleration is one toleration, field for field. It is short and bounded, so
// unlike the affinity structures there is nothing to reduce.
type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
	Seconds  *int64 `json:"toleration_seconds,omitempty"`
}

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
func reducePlacement(spec *corev1.PodSpec) (Placement, placementDrops) {
	var p Placement
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

func reduceNodeAffinity(na *corev1.NodeAffinity, drops *placementDrops) []NodeAffinityTerm {
	if na == nil {
		return nil
	}
	var out []NodeAffinityTerm
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
func appendSelectorTerm(out []NodeAffinityTerm, term corev1.NodeSelectorTerm, required bool, weight int32, drops *placementDrops) []NodeAffinityTerm {
	add := func(exprs []corev1.NodeSelectorRequirement) {
		for _, e := range exprs {
			if !fits(e.Key) {
				drops.Values++
				continue
			}
			t := NodeAffinityTerm{
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

func reducePodAffinity(pa *corev1.PodAffinity, drops *placementDrops) []TopologyTerm {
	if pa == nil {
		return nil
	}
	var out []TopologyTerm
	out = appendTopologyTerms(out, pa.RequiredDuringSchedulingIgnoredDuringExecution, drops)
	out = appendWeightedTopologyTerms(out, pa.PreferredDuringSchedulingIgnoredDuringExecution, drops)
	return capTerms(out, drops)
}

func reducePodAntiAffinity(pa *corev1.PodAntiAffinity, drops *placementDrops) []TopologyTerm {
	if pa == nil {
		return nil
	}
	var out []TopologyTerm
	out = appendTopologyTerms(out, pa.RequiredDuringSchedulingIgnoredDuringExecution, drops)
	out = appendWeightedTopologyTerms(out, pa.PreferredDuringSchedulingIgnoredDuringExecution, drops)
	return capTerms(out, drops)
}

func appendTopologyTerms(out []TopologyTerm, terms []corev1.PodAffinityTerm, drops *placementDrops) []TopologyTerm {
	for _, t := range terms {
		if !fits(t.TopologyKey) {
			drops.Values++
			continue
		}
		out = append(out, TopologyTerm{TopologyKey: t.TopologyKey, Required: true})
	}
	return out
}

func appendWeightedTopologyTerms(out []TopologyTerm, terms []corev1.WeightedPodAffinityTerm, drops *placementDrops) []TopologyTerm {
	for _, t := range terms {
		if !fits(t.PodAffinityTerm.TopologyKey) {
			drops.Values++
			continue
		}
		out = append(out, TopologyTerm{
			TopologyKey: t.PodAffinityTerm.TopologyKey,
			Weight:      t.Weight,
		})
	}
	return out
}

func reduceSpread(constraints []corev1.TopologySpreadConstraint, drops *placementDrops) []SpreadTerm {
	var out []SpreadTerm
	for _, c := range constraints {
		if !fits(c.TopologyKey) {
			drops.Values++
			continue
		}
		term := SpreadTerm{
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

func reduceTolerations(tolerations []corev1.Toleration, drops *placementDrops) []Toleration {
	var out []Toleration
	for _, t := range tolerations {
		if tolerationIsClusterDefault(t) {
			continue
		}
		if !fits(t.Key) || !fits(t.Value) {
			drops.Values++
			continue
		}
		kept := Toleration{
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
