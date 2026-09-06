package metrics

import (
	"sort"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/inventory"
	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofprobe"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofpull"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

// fullController is every optional block present at once, with a value in every
// field, so the tests below walk the whole exposition rather than the half an
// unconfigured agent produces.
func fullController() Controller {
	intake := model.IntakeRejections{Unauthorized: 1, TooLarge: 2, Malformed: 3}
	counters := inventory.Counters{Records: 4, NodesReported: 2, FactsReceived: 9, FactsJoined: 8}
	scan := inventory.ScanCoverage{
		Nodes: 2, OldestAssertedAt: time.Unix(1_700_000_000, 0),
		ProcessesScanned: 100, GoFound: 10, FilteredScope: 1, FilteredInfra: 2, Unreadable: 3,
	}
	ebpf := inventory.ProfileCoverage{
		Nodes: 2, States: map[string]int{"supported": 1, "btf_absent": 1},
		Windows: 5, WindowsNoScope: 1, ProfilesShipped: 4, ThirdPartyDropped: 7,
	}
	probe := pprofprobe.Coverage{Confirmed: 3, Absent: 2, Unreachable: 1}
	pull := pprofpull.Coverage{Shipped: 2, Refused: 1, Unreachable: 1, Invalid: 1}
	spool := sink.Counters{
		Written:       map[string]int64{},
		WriteFailures: map[string]int64{},
		Evicted:       map[string]int64{},
		Bytes:         1024, Files: 12,
	}
	for _, k := range sink.Registry() {
		spool.Written[k.Kind] = 1
		spool.WriteFailures[k.Kind] = 0
	}
	for _, r := range sink.EvictionReasons {
		spool.Evicted[r] = 1
	}
	return Controller{
		Process: Process{Role: RoleController, Version: "1.2.3",
			PassStart: time.Unix(1_700_000_100, 0), Deadline: 3 * time.Minute},
		Sources: []model.SourceHealth{{Name: "services", Synced: true}, {Name: "nodes", Failing: true}},
		Kubelet: []collector.PathReads{
			{Path: collector.PathStatsSummary, Attempted: 10, Failed: 1},
			{Path: collector.PathMetricsCadvisor, Attempted: 10, Failed: 4},
		},
		Filter:           model.Coverage{PodsObserved: 412, ExcludedNamespaceFilter: 37, JobsObserved: 9},
		Placement:        model.PlacementDrops{Values: 1, Terms: 2},
		Nodes:            model.NodeDrops{Conditions: 1, Devices: 2, Taints: 3, Values: 4},
		Spool:            spool,
		Intake:           &intake,
		Inventory:        &counters,
		Scan:             &scan,
		EBPF:             &ebpf,
		Probe:            &probe,
		Pull:             &pull,
		ProfilesReceived: 6,
		ProfilesUnjoined: 1,
	}
}

func fullNode() Node {
	return Node{
		Process: Process{Role: RoleNode, Version: "1.2.3",
			PassStart: time.Unix(1_700_000_100, 0), Deadline: 4 * time.Minute},
		Scanned:     true,
		Scan:        nodescan.Counters{ProcessesScanned: 80, GoFound: 5, FilteredInfra: 3, Unreadable: 2},
		PodsInScope: 12,
		Profiling: nodescan.ProfilingCoverage{
			State: "supported", Windows: 3, WindowsNoTargets: 1,
			ProfilesShipped: 2, SamplesFiltered: 11,
		},
		ReportsShipped: 7,
		ReportFailures: 2,
	}
}

func families(t *testing.T, s *Set) []*dto.MetricFamily {
	t.Helper()
	out, err := s.Families()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	return out
}

// TestTheRealExpositionCarriesOnlyPermittedLabels is the load-bearing one: it
// walks what the agent actually serves, not the builder's own guard, so a
// metric added with a label outside the set fails even if it never goes through
// a hand-written literal.
func TestTheRealExpositionCarriesOnlyPermittedLabels(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  *Set
	}{
		{"controller", CollectController(fullController())},
		{"node", CollectNode(fullNode())},
	} {
		for _, fam := range families(t, tc.set) {
			for _, m := range fam.GetMetric() {
				for _, l := range m.GetLabel() {
					if !Allowed[LabelName(l.GetName())] {
						t.Errorf("%s: metric %q carries label %q, which is not permitted",
							tc.name, fam.GetName(), l.GetName())
					}
				}
			}
		}
	}
}

// TestNoLabelValueNamesAnythingFromTheCluster: the label names are closed, and
// their values are drawn from enumerations in the code — never from a string a
// cluster object supplied. This asserts it against a controller whose sources
// hold values that would be identities if any of them leaked through.
func TestNoLabelValueNamesAnythingFromTheCluster(t *testing.T) {
	c := fullController()
	// A node reporting a state this build does not know must not be able to
	// invent a series in the controller's exposition (ADR 0070 §3).
	c.EBPF.States = map[string]int{"prod-cluster-node-17": 1, "supported": 1}

	permitted := map[string]bool{}
	for _, v := range ProfilingStates {
		permitted[v] = true
	}
	for _, v := range []string{
		StateOther, RoleController, "1.2.3", "services", "nodes",
		string(collector.PathStatsSummary), string(collector.PathMetricsCadvisor),
		"namespace_filter", "namespace_annotation", "workload_annotation", "pod_annotation",
		"job_annotation", "unknown_kind", "not_cached",
		"placement_values", "placement_terms", "node_conditions", "node_devices",
		"node_taints", "node_values",
		"unauthorized", "too_large", "malformed",
		"received", "joined", "unjoined", "undigested",
		"scanned", "go_found", "filtered_scope", "filtered_infra", "unreadable",
		"no_scope", "no_targets", "no_samples", "shipped", "invalid", "unshipped",
		"out_of_scope", "third_party", "unsymbolized", "filtered",
		"confirmed", "absent", "unreachable", "refused",
	} {
		permitted[v] = true
	}
	for _, k := range sink.Registry() {
		permitted[k.Kind] = true
	}
	for _, r := range sink.EvictionReasons {
		permitted[r] = true
	}

	for _, fam := range families(t, CollectController(c)) {
		for _, m := range fam.GetMetric() {
			for _, l := range m.GetLabel() {
				if !permitted[l.GetValue()] {
					t.Errorf("metric %q carries label value %q, which comes from no enumeration in this package",
						fam.GetName(), l.GetValue())
				}
			}
		}
	}
}

// TestThePayloadKindLabelIsTheRegistry: the label's values are the registry's
// rows, all of them, including the kinds this agent has never written. A kind
// that never ships reading as a zero is the whole point — "no usage_window has
// been written in an hour" is the alert (ADR 0070 §3).
func TestThePayloadKindLabelIsTheRegistry(t *testing.T) {
	c := fullController()
	// A spool that has written nothing at all still names every kind, because
	// its counters are seeded from the registry.
	c.Spool.Written = map[string]int64{}
	for _, k := range sink.Registry() {
		c.Spool.Written[k.Kind] = 0
	}

	var got []string
	for _, fam := range families(t, CollectController(c)) {
		if fam.GetName() != Prefix+"spool_payloads_written_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == string(LabelKind) {
					got = append(got, l.GetValue())
				}
			}
		}
	}
	var want []string
	for _, k := range sink.Registry() {
		want = append(want, k.Kind)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the kind label exposes\n  %v\nand the registry holds\n  %v\n"+
			"the registry is the one list (ADR 0022)", got, want)
	}
}

// TestAnUnconfiguredControllerOmitsWhatItDoesNotHave: a metrics-only install
// has no node channel, no inventory and no profiling, and must not report zeros
// for them — a zero is a claim, and "no DaemonSet" is not "no profiles".
func TestAnUnconfiguredControllerOmitsWhatItDoesNotHave(t *testing.T) {
	bare := Controller{
		Process: Process{Role: RoleController, Version: "dev"},
		Spool:   sink.Counters{Written: map[string]int64{}, WriteFailures: map[string]int64{}, Evicted: map[string]int64{}},
	}
	present := map[string]bool{}
	for _, fam := range families(t, CollectController(bare)) {
		present[fam.GetName()] = true
	}
	for _, absent := range []string{
		"node_reports_rejected_total", "nodes_reporting", "inventory_records",
		"inventory_facts_total", "scan_processes", "ebpf_nodes", "ebpf_windows",
		"pprof_targets", "pprof_pulls_total", "node_report_oldest_timestamp_seconds",
	} {
		if present[Prefix+absent] {
			t.Errorf("%s%s is exposed by an installation that has no such component", Prefix, absent)
		}
	}
	for _, required := range []string{"build_info", "pass_start_timestamp_seconds", "spool_bytes"} {
		if !present[Prefix+required] {
			t.Errorf("%s%s is missing; every installation has it", Prefix, required)
		}
	}
}

// TestTheLivenessStampIsTheOneTheProbeReads: the alert an operator writes on
// this metric must fire on the same condition the kubelet restarts the pod for,
// so both numbers come from the heartbeat rather than from a second clock.
func TestTheLivenessStampIsTheOneTheProbeReads(t *testing.T) {
	c := fullController()
	got := map[string]float64{}
	for _, fam := range families(t, CollectController(c)) {
		for _, m := range fam.GetMetric() {
			got[fam.GetName()] = m.GetGauge().GetValue()
		}
	}
	if want := float64(c.Process.PassStart.Unix()); got[Prefix+"pass_start_timestamp_seconds"] != want {
		t.Errorf("pass stamp is %v, want %v", got[Prefix+"pass_start_timestamp_seconds"], want)
	}
	if want := c.Process.Deadline.Seconds(); got[Prefix+"pass_deadline_seconds"] != want {
		t.Errorf("deadline is %v, want %v", got[Prefix+"pass_deadline_seconds"], want)
	}
}

// TestCountersAndGaugesAreTypedByWhetherTheyCanFall. Anything summed over the
// fleet's latest reports drops when a node leaves, so it is a gauge; only a
// number this process owns and never lowers is a counter, and only those carry
// the _total suffix.
func TestCountersAndGaugesAreTypedByWhetherTheyCanFall(t *testing.T) {
	for _, set := range []*Set{CollectController(fullController()), CollectNode(fullNode())} {
		for _, fam := range families(t, set) {
			total := strings.HasSuffix(fam.GetName(), "_total")
			counter := fam.GetType() == dto.MetricType_COUNTER
			if total != counter {
				t.Errorf("%q is a %s: the _total suffix and the counter type must agree",
					fam.GetName(), fam.GetType())
			}
		}
	}
}

// TestANodeStateOutsideTheEnumerationBecomesOther. The state crosses the
// channel from a node, and an unrecognized one must not open a label value of
// its own (ADR 0060 §2, ADR 0070 §3).
func TestANodeStateOutsideTheEnumerationBecomesOther(t *testing.T) {
	n := fullNode()
	n.Profiling.State = "something-a-newer-node-invented"
	for _, fam := range families(t, CollectNode(n)) {
		if fam.GetName() != Prefix+"ebpf_state" {
			continue
		}
		for _, m := range fam.GetMetric() {
			if v := m.GetLabel()[0].GetValue(); v != StateOther {
				t.Errorf("unknown state exposed as %q, want %q", v, StateOther)
			}
		}
	}

	c := fullController()
	c.EBPF.States = map[string]int{"invented": 2, "supported": 1}
	for _, fam := range families(t, CollectController(c)) {
		if fam.GetName() != Prefix+"ebpf_nodes" {
			continue
		}
		for _, m := range fam.GetMetric() {
			if m.GetLabel()[0].GetValue() == StateOther && m.GetGauge().GetValue() != 2 {
				t.Errorf("the two nodes in an unknown state did not land in %q", StateOther)
			}
		}
	}
}

// TestANodeSaysNothingAboutAScanItHasNotRun: zeroes before the first pass would
// read as a node that walked an empty process table.
func TestANodeSaysNothingAboutAScanItHasNotRun(t *testing.T) {
	n := Node{Process: Process{Role: RoleNode, Version: "dev"}}
	for _, fam := range families(t, CollectNode(n)) {
		switch fam.GetName() {
		case Prefix + "scan_processes", Prefix + "scan_pods_in_scope":
			t.Errorf("%s is exposed before the first pass finished", fam.GetName())
		}
	}
	// What it does say is that it is alive and how it is configured, which is
	// exactly the state a node in its first pass is in.
	present := map[string]bool{}
	for _, fam := range families(t, CollectNode(n)) {
		present[fam.GetName()] = true
	}
	for _, required := range []string{"build_info", "pass_start_timestamp_seconds", "ebpf_state"} {
		if !present[Prefix+required] {
			t.Errorf("%s%s is missing", Prefix, required)
		}
	}
}
