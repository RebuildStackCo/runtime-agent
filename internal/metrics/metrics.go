// Package metrics exposes what the agent did about its own work, in the
// Prometheus text format, for the customer's own monitoring.
//
// Two rules shape the whole package (ADR 0070). Every number here is read from
// the same accessor `collection_coverage` is built from, so the two audiences
// cannot be given different answers. And a label name comes from a closed set,
// so the endpoint can neither grow unbounded series nor become a second channel
// for identities the payloads do not carry (CLAUDE.md invariant 6).
package metrics

import (
	"fmt"
	"sort"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

// Prefix every metric this agent exposes carries. It names the component being
// observed, which is what a prefix is for, and matches the chart and image.
const Prefix = "runtime_agent_"

// LabelName is a label this agent may attach. The constants below are the whole
// permitted set; Allowed is what enforces it, and TestOnlyTheseLabelNamesAreAllowed
// pins the set so widening it is a decision in a diff.
//
// No value of any of these ever holds a namespace, pod, node, workload or image
// name: the closed set is a cardinality budget and a disclosure boundary at once.
type LabelName string

const (
	// LabelRole is "controller" or "node", and LabelVersion the agent build.
	// Both name the agent, never the cluster, and each takes one value per
	// process lifetime.
	LabelRole LabelName = "role"
	// LabelVersion is the agent build.
	LabelVersion LabelName = "version"
	// LabelSource is a watched resource class ("services", "nodes"), as
	// model.SourceHealth already reports it.
	LabelSource LabelName = "source"
	// LabelPath is one of the two kubelet stats paths (ADR 0013 §3).
	LabelPath LabelName = "path"
	// LabelReason is why something was excluded, evicted or refused.
	LabelReason LabelName = "reason"
	// LabelOutcome is which of several ends a piece of work reached.
	LabelOutcome LabelName = "outcome"
	// LabelKind is a payload kind, and its values come from
	// internal/sink.Registry() rather than from a list kept here.
	LabelKind LabelName = "kind"
	// LabelState is a node's profiling state, mapped through the enumeration in
	// ADR 0060 §2 because it arrives over the wire, or a pprof target's answer.
	LabelState LabelName = "state"
	// LabelSubject is which reduction dropped a value (ADR 0031, ADR 0064 §4).
	LabelSubject LabelName = "subject"
)

// Allowed is the closed set of label names, as a set for the check below and
// for the test that pins it.
var Allowed = map[LabelName]bool{
	LabelRole: true, LabelVersion: true, LabelSource: true, LabelPath: true,
	LabelReason: true, LabelOutcome: true, LabelKind: true, LabelState: true,
	LabelSubject: true,
}

// Label is one dimension of one series.
type Label struct {
	Name  LabelName
	Value string
}

// L builds a label. It exists so a call site reads as one argument per
// dimension rather than as a struct literal per dimension.
func L(name LabelName, value string) Label { return Label{Name: name, Value: value} }

// Set is metric families under construction. It is filled per scrape from
// snapshots and thrown away — there is deliberately no long-lived registry of
// counter objects to increment, because that is the shape in which a second
// counter appears beside the one the payload reads (ADR 0070 §2).
type Set struct {
	order    []string
	families map[string]*dto.MetricFamily
	err      error
}

// NewSet returns an empty set.
func NewSet() *Set {
	return &Set{families: make(map[string]*dto.MetricFamily)}
}

// Gauge adds one gauge series. A gauge is the right type for anything that can
// fall — including every count summed over the fleet's latest node reports,
// which drops when a node leaves (ADR 0067).
func (s *Set) Gauge(name, help string, value float64, labels ...Label) {
	s.add(name, help, dto.MetricType_GAUGE, &dto.Metric{Gauge: &dto.Gauge{Value: proto.Float64(value)}}, labels)
}

// Counter adds one counter series. Only for a number this process increments
// and never lowers; the name must end in _total.
func (s *Set) Counter(name, help string, value float64, labels ...Label) {
	s.add(name, help, dto.MetricType_COUNTER, &dto.Metric{Counter: &dto.Counter{Value: proto.Float64(value)}}, labels)
}

func (s *Set) add(name, help string, typ dto.MetricType, m *dto.Metric, labels []Label) {
	full := Prefix + name
	for _, l := range labels {
		if !Allowed[l.Name] {
			s.fail(fmt.Errorf("metric %s: label name %q is not in the permitted set (ADR 0070 §3)", full, l.Name))
			return
		}
		m.Label = append(m.Label, &dto.LabelPair{
			Name:  proto.String(string(l.Name)),
			Value: proto.String(l.Value),
		})
	}
	sort.Slice(m.Label, func(i, j int) bool { return m.Label[i].GetName() < m.Label[j].GetName() })

	fam, ok := s.families[full]
	if !ok {
		fam = &dto.MetricFamily{Name: proto.String(full), Help: proto.String(help), Type: typ.Enum()}
		s.families[full] = fam
		s.order = append(s.order, full)
	} else if fam.GetType() != typ {
		s.fail(fmt.Errorf("metric %s is exposed as both %s and %s", full, fam.GetType(), typ))
		return
	}
	fam.Metric = append(fam.Metric, m)
}

func (s *Set) fail(err error) {
	if s.err == nil {
		s.err = err
	}
}

// Families returns the set in a stable order, or the first refusal. A refused
// label name yields no exposition at all rather than a partial one: the caller
// serves an error, and the test that pins the permitted set is what keeps this
// unreachable.
func (s *Set) Families() ([]*dto.MetricFamily, error) {
	if s.err != nil {
		return nil, s.err
	}
	sort.Strings(s.order)
	out := make([]*dto.MetricFamily, 0, len(s.order))
	for _, name := range s.order {
		out = append(out, s.families[name])
	}
	return out, nil
}
