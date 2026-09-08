package metrics

import (
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/inventory"
	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofprobe"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofpull"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

// The two roles, as the `role` label carries them.
const (
	RoleController = "controller"
	RoleNode       = "node"
)

// ProfilingStates is the enumeration a node's profiling state is mapped
// through, from ADR 0060 §2. The state arrives over the wire from a node, so an
// unrecognized one becomes StateOther rather than a label value of its own: a
// node is not permitted to invent series in the controller's exposition.
var ProfilingStates = []string{
	"supported", "disabled", "program_load_failed", "capture_stopped",
	"kernel_too_old", "btf_absent", "kernel_unknown",
}

// StateOther is where a state outside the enumeration lands.
const StateOther = "other"

func profilingState(state string) string {
	for _, known := range ProfilingStates {
		if state == known {
			return state
		}
	}
	return StateOther
}

// Process is what both roles say about themselves: which build is running, and
// whether the loop that does the work is still turning. The stamp and its
// deadline are the ones /livez reads (ADR 0069 §2), so an alert on this metric
// fires on exactly the condition the kubelet restarts the pod for — and, unlike
// the probe, says so before the restart erases the evidence.
type Process struct {
	Role      string
	Version   string
	PassStart time.Time
	Deadline  time.Duration
}

func (s *Set) process(p Process) {
	s.Gauge("build_info", "The agent build that is running, always 1.", 1,
		L(LabelRole, p.Role), L(LabelVersion, p.Version))
	s.Gauge("pass_start_timestamp_seconds",
		"When the role's own loop last reached the top of a pass.",
		float64(p.PassStart.Unix()))
	s.Gauge("pass_deadline_seconds",
		"How stale that stamp may be before the role reports itself not alive.",
		p.Deadline.Seconds())
}

// Controller is everything the controller exposes about its own work. Every
// field is a snapshot the coverage payload is also built from; nothing here is
// counted twice (ADR 0070 §2).
type Controller struct {
	Process   Process
	Sources   []model.SourceHealth
	Kubelet   []collector.PathReads
	Filter    model.Coverage
	Placement model.PlacementDrops
	Nodes     model.NodeDrops
	Spool     sink.Counters
	// Optional blocks, present when the feature they describe is installed.
	Intake           *model.IntakeRejections
	Inventory        *inventory.Counters
	Scan             *inventory.ScanCoverage
	EBPF             *inventory.ProfileCoverage
	Probe            *pprofprobe.Coverage
	Pull             *pprofpull.Coverage
	Shipping         *model.Shipping
	ProfilesReceived uint64
	ProfilesUnjoined uint64
}

// CollectController builds the controller's exposition.
func CollectController(c Controller) *Set {
	s := NewSet()
	s.process(c.Process)

	for _, src := range c.Sources {
		s.Gauge("source_synced", "Whether a watched source's cache ever filled.",
			b2f(src.Synced), L(LabelSource, src.Name))
		s.Gauge("source_failing", "Whether a watched source's watch is erroring (ADR 0035).",
			b2f(src.Failing), L(LabelSource, src.Name))
	}

	for _, r := range c.Kubelet {
		s.Counter("kubelet_requests_total", "Kubelet stats requests attempted, by path.",
			float64(r.Attempted), L(LabelPath, string(r.Path)))
		s.Counter("kubelet_request_failures_total", "Kubelet stats requests that failed, by path.",
			float64(r.Failed), L(LabelPath, string(r.Path)))
	}

	f := c.Filter
	s.Counter("pods_observed_total", "Pod appearances the filter considered.", float64(f.PodsObserved))
	excluded := "Pod appearances excluded from collection, by which control excluded them."
	s.Counter("pods_excluded_total", excluded, float64(f.ExcludedNamespaceFilter), L(LabelReason, "namespace_filter"))
	s.Counter("pods_excluded_total", excluded, float64(f.ExcludedNamespaceAnnotation), L(LabelReason, "namespace_annotation"))
	s.Counter("pods_excluded_total", excluded, float64(f.ExcludedWorkloadAnnotation), L(LabelReason, "workload_annotation"))
	s.Counter("pods_excluded_total", excluded, float64(f.ExcludedPodAnnotation), L(LabelReason, "pod_annotation"))
	s.Counter("pods_excluded_from_profiling_total",
		"Collected pods the profiling opt-out excluded (ADR 0071).",
		float64(f.ExcludedProfilingAnnotation))
	unchecked := "Pods collected without their workload's opt-out being checked (ADR 0028)."
	s.Counter("pods_optout_unchecked_total", unchecked, float64(f.WorkloadUnknownKind), L(LabelReason, "unknown_kind"))
	s.Counter("pods_optout_unchecked_total", unchecked, float64(f.WorkloadNotCached), L(LabelReason, "not_cached"))

	s.Counter("jobs_observed_total", "Finished Job runs the filter considered.", float64(f.JobsObserved))
	jobsExcluded := "Job runs excluded from collection, by which control excluded them."
	s.Counter("jobs_excluded_total", jobsExcluded, float64(f.JobsExcludedNamespaceFilter), L(LabelReason, "namespace_filter"))
	s.Counter("jobs_excluded_total", jobsExcluded, float64(f.JobsExcludedNamespaceAnnotation), L(LabelReason, "namespace_annotation"))
	s.Counter("jobs_excluded_total", jobsExcluded, float64(f.JobsExcludedWorkloadAnnotation), L(LabelReason, "workload_annotation"))
	s.Counter("jobs_excluded_total", jobsExcluded, float64(f.JobsExcludedAnnotation), L(LabelReason, "job_annotation"))

	drops := "Values the reductions refused to carry into a payload (ADR 0031, ADR 0064)."
	s.Counter("reduction_drops_total", drops, float64(c.Placement.Values), L(LabelSubject, "placement_values"))
	s.Counter("reduction_drops_total", drops, float64(c.Placement.Terms), L(LabelSubject, "placement_terms"))
	s.Counter("reduction_drops_total", drops, float64(c.Nodes.Conditions), L(LabelSubject, "node_conditions"))
	s.Counter("reduction_drops_total", drops, float64(c.Nodes.Devices), L(LabelSubject, "node_devices"))
	s.Counter("reduction_drops_total", drops, float64(c.Nodes.Taints), L(LabelSubject, "node_taints"))
	s.Counter("reduction_drops_total", drops, float64(c.Nodes.Values), L(LabelSubject, "node_values"))

	s.spool(c.Spool)

	if r := c.Intake; r != nil {
		refused := "Node reports the receiver refused, by reason (ADR 0067)."
		s.Counter("node_reports_rejected_total", refused, float64(r.Unauthorized), L(LabelReason, "unauthorized"))
		s.Counter("node_reports_rejected_total", refused, float64(r.TooLarge), L(LabelReason, "too_large"))
		s.Counter("node_reports_rejected_total", refused, float64(r.Malformed), L(LabelReason, "malformed"))
	}

	if ic := c.Inventory; ic != nil {
		s.Gauge("nodes_reporting", "Nodes that have delivered a scan report.", float64(ic.NodesReported))
		s.Gauge("inventory_records", "Distinct (namespace, workload, container) inventory records held.",
			float64(ic.Records))
		facts := "On-node build facts received, and what became of each (ADR 0010 §5)."
		s.Counter("inventory_facts_total", facts, float64(ic.FactsReceived), L(LabelOutcome, "received"))
		s.Counter("inventory_facts_total", facts, float64(ic.FactsJoined), L(LabelOutcome, "joined"))
		s.Counter("inventory_facts_total", facts, float64(ic.FactsUnjoined), L(LabelOutcome, "unjoined"))
		s.Counter("inventory_facts_total", facts, float64(ic.FactsUndigested), L(LabelOutcome, "undigested"))
	}

	if sc := c.Scan; sc != nil {
		// A node that stops reporting keeps contributing its last pass, so this
		// instant is what says one went quiet — the sums beside it cannot
		// (ADR 0067). Zero before any node has reported.
		if !sc.OldestAssertedAt.IsZero() {
			s.Gauge("node_report_oldest_timestamp_seconds",
				"When the stalest reporting node last stated its contribution.",
				float64(sc.OldestAssertedAt.Unix()))
		}
		s.scanProcesses(nodescan.Counters{
			ProcessesScanned: sc.ProcessesScanned,
			GoFound:          sc.GoFound,
			FilteredScope:    sc.FilteredScope,
			FilteredInfra:    sc.FilteredInfra,
			Unreadable:       sc.Unreadable,
		})
	}

	if pc := c.EBPF; pc != nil {
		byState := map[string]int{}
		for state, n := range pc.States {
			byState[profilingState(state)] += n
		}
		for _, state := range append(append([]string{}, ProfilingStates...), StateOther) {
			s.Gauge("ebpf_nodes", "Reporting nodes in each profiling state (ADR 0060 §2).",
				float64(byState[state]), L(LabelState, state))
		}
		s.ebpf(nodescan.ProfilingCoverage{
			Windows: pc.Windows, WindowsNoScope: pc.WindowsNoScope,
			WindowsNoTargets: pc.WindowsNoTargets, WindowsNoSamples: pc.WindowsNoSamples,
			ProfilesShipped: pc.ProfilesShipped, ProfilesInvalid: pc.ProfilesInvalid,
			ProfilesUnshipped: pc.ProfilesUnshipped, SamplesOutOfScope: pc.SamplesOutOfScope,
			ThirdPartyDropped: pc.ThirdPartyDropped, UnsymbolizedDropped: pc.UnsymbolizedDropped,
			SamplesFiltered: pc.SamplesFiltered,
		})
		// The controller's own two counts: a profile a node captured and this
		// side could not join to a workload is lost here, after the work was done.
		s.Counter("ebpf_profiles_received_total", "Captured profiles that reached the controller.",
			float64(c.ProfilesReceived))
		s.Counter("ebpf_profiles_unjoined_total", "Captured profiles dropped for want of a pod to join them to.",
			float64(c.ProfilesUnjoined))
	}

	if p := c.Probe; p != nil {
		targets := "Workload endpoints by their state in the pprof funnel (ADR 0057, ADR 0071)."
		s.Gauge("pprof_targets", targets, float64(p.Confirmed), L(LabelState, "confirmed"))
		s.Gauge("pprof_targets", targets, float64(p.Absent), L(LabelState, "absent"))
		s.Gauge("pprof_targets", targets, float64(p.Unreachable), L(LabelState, "unreachable"))
		s.Gauge("pprof_targets", targets, float64(p.ExcludedByAnnotation),
			L(LabelState, "excluded_by_annotation"))
	}

	if p := c.Pull; p != nil {
		pulls := "Profile pulls by outcome; `refused` is a workload running its own profiler (ADR 0058 §3)."
		s.Counter("pprof_pulls_total", pulls, float64(p.Shipped), L(LabelOutcome, "shipped"))
		s.Counter("pprof_pulls_total", pulls, float64(p.Refused), L(LabelOutcome, "refused"))
		s.Counter("pprof_pulls_total", pulls, float64(p.Unreachable), L(LabelOutcome, "unreachable"))
		s.Counter("pprof_pulls_total", pulls, float64(p.Invalid), L(LabelOutcome, "invalid"))
	}

	if sh := c.Shipping; sh != nil {
		shipments := "Payloads offered to the backend, by what became of each (ADR 0075)."
		s.Counter("shipments_total", shipments, float64(sh.Delivered), L(LabelOutcome, "delivered"))
		s.Counter("shipments_total", shipments, float64(sh.Deferred), L(LabelOutcome, "deferred"))
		s.Counter("shipments_total", shipments, float64(sh.Unreadable), L(LabelOutcome, "unreadable"))
		refused := "Payloads the backend refused permanently, by reason. None is offered again."
		s.Counter("shipments_rejected_total", refused, float64(sh.Rejected.Unauthorized), L(LabelReason, "unauthorized"))
		s.Counter("shipments_rejected_total", refused, float64(sh.Rejected.TooLarge), L(LabelReason, "too_large"))
		s.Counter("shipments_rejected_total", refused, float64(sh.Rejected.Malformed), L(LabelReason, "malformed"))
		// The one number a halted agent can still deliver: it ships nothing, so
		// the coverage payload carrying the same fact never arrives (ADR 0075).
		s.Gauge("shipping_halted",
			"Whether shipping has stopped on an identity failure the agent will not retry into.",
			b2f(sh.Halted))
	}
	return s
}

func (s *Set) spool(c sink.Counters) {
	for _, kind := range sink.SortedKeys(c.Written) {
		s.Counter("spool_payloads_written_total", "Payloads written to the spool, by kind.",
			float64(c.Written[kind]), L(LabelKind, kind))
	}
	for _, kind := range sink.SortedKeys(c.WriteFailures) {
		s.Counter("spool_write_failures_total", "Spool writes that failed, by payload kind.",
			float64(c.WriteFailures[kind]), L(LabelKind, kind))
	}
	for _, kind := range sink.SortedKeys(c.Suppressed) {
		s.Counter("spool_payloads_suppressed_total",
			"Payloads a pass assembled and the kind's cadence did not write, by kind (ADR 0073).",
			float64(c.Suppressed[kind]), L(LabelKind, kind))
	}
	for _, reason := range sink.SortedKeys(c.Evicted) {
		s.Counter("spool_evicted_total",
			"Payloads the sweep removed, by which bound removed them (ADR 0042).",
			float64(c.Evicted[reason]), L(LabelReason, reason))
	}
	s.Gauge("spool_bytes", "Bytes of payload the spool held at the last sweep.", float64(c.Bytes))
	s.Gauge("spool_files", "Payload files the spool held at the last sweep.", float64(c.Files))
	s.Gauge("spool_max_bytes", "The spool's byte ceiling.", float64(sink.DefaultMaxBytes))
	s.Gauge("spool_max_files", "The spool's file-count ceiling.", float64(sink.DefaultMaxFiles))
	// Gauges, not counters: the startup read happens once and these never move
	// again. Zero windows after a restart is the state ADR 0072 exists to
	// prevent, and this is the only place it is visible from outside.
	s.Gauge("spool_windows_resumed",
		"Open usage windows this process resumed from the spool at startup (ADR 0072).",
		float64(c.Recovered.Windows))
	s.Gauge("spool_records_resumed", "Records those resumed windows carried.",
		float64(c.Recovered.Records))
	s.Gauge("spool_recovery_files_skipped",
		"Spool files the startup read could not use. They were left where they are.",
		float64(c.Recovered.Skipped))
}

// scanProcesses is one scan pass's process counts — the fleet's latest passes
// summed on the controller, this node's latest on a node. A gauge either way:
// they describe one walk of a process table, not a running total (ADR 0054 §4).
func (s *Set) scanProcesses(c nodescan.Counters) {
	walked := "Processes the latest scan pass walked, and what became of each."
	s.Gauge("scan_processes", walked, float64(c.ProcessesScanned), L(LabelOutcome, "scanned"))
	s.Gauge("scan_processes", walked, float64(c.GoFound), L(LabelOutcome, "go_found"))
	s.Gauge("scan_processes", walked, float64(c.FilteredScope), L(LabelOutcome, "filtered_scope"))
	s.Gauge("scan_processes", walked, float64(c.FilteredInfra), L(LabelOutcome, "filtered_infra"))
	s.Gauge("scan_processes", walked, float64(c.Unreadable), L(LabelOutcome, "unreadable"))
}

// ebpf is what a profiler did. Gauges rather than counters on the controller,
// because the sum is over the nodes reporting now and falls when one leaves; the
// same shape is used on the node, where the numbers reset with the process.
func (s *Set) ebpf(c nodescan.ProfilingCoverage) {
	s.Gauge("ebpf_windows", "Capture windows cut.", float64(c.Windows))
	empty := "Capture windows that produced nothing, by reason (ADR 0060)."
	s.Gauge("ebpf_windows_empty", empty, float64(c.WindowsNoScope), L(LabelReason, "no_scope"))
	s.Gauge("ebpf_windows_empty", empty, float64(c.WindowsNoTargets), L(LabelReason, "no_targets"))
	s.Gauge("ebpf_windows_empty", empty, float64(c.WindowsNoSamples), L(LabelReason, "no_samples"))
	profiles := "Profiles built, by what became of each."
	s.Gauge("ebpf_profiles", profiles, float64(c.ProfilesShipped), L(LabelOutcome, "shipped"))
	s.Gauge("ebpf_profiles", profiles, float64(c.ProfilesInvalid), L(LabelOutcome, "invalid"))
	s.Gauge("ebpf_profiles", profiles, float64(c.ProfilesUnshipped), L(LabelOutcome, "unshipped"))
	dropped := "Samples and frames dropped, by reason; third_party rising means an allow-list that stopped matching (ADR 0059)."
	s.Gauge("ebpf_samples_dropped", dropped, float64(c.SamplesOutOfScope), L(LabelReason, "out_of_scope"))
	s.Gauge("ebpf_samples_dropped", dropped, float64(c.ThirdPartyDropped), L(LabelReason, "third_party"))
	s.Gauge("ebpf_samples_dropped", dropped, float64(c.UnsymbolizedDropped), L(LabelReason, "unsymbolized"))
	s.Gauge("ebpf_samples_dropped", dropped, float64(c.SamplesFiltered), L(LabelReason, "filtered"))
}

// Node is what one node pod exposes. It answers the question the coverage
// payload deliberately cannot — which node — because the asking is done by the
// customer's own service discovery rather than by a label the agent attaches
// (ADR 0054 §4, ADR 0070 §4).
type Node struct {
	Process Process
	// Scanned is whether a pass has finished. Before one has, the scan counts
	// are absent rather than zero: a zero is a claim, and "this node walked an
	// empty process table" is not what a node that has not yet walked one means.
	Scanned        bool
	Scan           nodescan.Counters
	PodsInScope    int
	Profiling      nodescan.ProfilingCoverage
	ReportsShipped uint64
	ReportFailures uint64
}

// CollectNode builds one node's exposition.
func CollectNode(n Node) *Set {
	s := NewSet()
	s.process(n.Process)
	if n.Scanned {
		s.scanProcesses(n.Scan)
		s.Gauge("scan_pods_in_scope", "Pods the controller admitted this node to scan (ADR 0015).",
			float64(n.PodsInScope))
	}
	s.Gauge("ebpf_state", "This node's profiling state, always 1 for the state it is in.", 1,
		L(LabelState, profilingState(n.Profiling.State)))
	s.ebpf(n.Profiling)
	s.Counter("node_reports_shipped_total", "Scan reports this node delivered to the controller.",
		float64(n.ReportsShipped))
	s.Counter("node_report_failures_total", "Scan report deliveries that failed.",
		float64(n.ReportFailures))
	return s
}

func b2f(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
