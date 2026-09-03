package model

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

// Toleration is one toleration, field for field. It is short and bounded, so
// unlike the affinity structures there is nothing to reduce.
type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
	Seconds  *int64 `json:"toleration_seconds,omitempty"`
}
