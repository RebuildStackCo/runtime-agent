package main

import (
	dto "github.com/prometheus/client_model/go"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/health"
	"github.com/RebuildStackCo/runtime-agent/internal/inventory"
	"github.com/RebuildStackCo/runtime-agent/internal/metrics"
	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofprobe"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofpull"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

// controllerSources is where the controller's metrics come from: the same
// accessors `logCoverage` reads to build `collection_coverage`, held as
// functions so the two are one set of counters read twice rather than two sets
// (ADR 0070 §2). A nil function is a feature this installation does not have.
type controllerSources struct {
	beat      *health.Heartbeat
	sources   func() []model.SourceHealth
	kubelet   func() []collector.PathReads
	filter    func() model.Coverage
	placement func() model.PlacementDrops
	nodes     func() model.NodeDrops
	spool     func() sink.Counters

	// intake is late-bound: the receiver is wired after this listener is, so
	// the presence of one is a question asked per scrape, not at startup.
	intake    func() (model.IntakeRejections, bool)
	inventory func() (inventory.Counters, inventory.ScanCoverage)
	ebpf      func() (inventory.ProfileCoverage, uint64, uint64)
	probe     func() pprofprobe.Coverage
	pull      func() pprofpull.Coverage
}

func (c controllerSources) gather() ([]*dto.MetricFamily, error) {
	m := metrics.Controller{
		Process: metrics.Process{
			Role:      metrics.RoleController,
			Version:   version,
			PassStart: c.beat.Last(),
			Deadline:  c.beat.Deadline(),
		},
		Sources:   c.sources(),
		Kubelet:   c.kubelet(),
		Filter:    c.filter(),
		Placement: c.placement(),
		Nodes:     c.nodes(),
		Spool:     c.spool(),
	}
	if c.intake != nil {
		if r, ok := c.intake(); ok {
			m.Intake = &r
		}
	}
	if c.inventory != nil {
		counters, scan := c.inventory()
		m.Inventory, m.Scan = &counters, &scan
	}
	if c.ebpf != nil {
		// Present only once a node has said what its profiler did, exactly as
		// in the coverage payload: before that there is no fleet to describe.
		if cov, received, unjoined := c.ebpf(); cov.Nodes > 0 {
			m.EBPF, m.ProfilesReceived, m.ProfilesUnjoined = &cov, received, unjoined
		}
	}
	if c.probe != nil {
		p := c.probe()
		m.Probe = &p
	}
	if c.pull != nil {
		p := c.pull()
		m.Pull = &p
	}
	return metrics.CollectController(m).Families()
}

// nodeSources is the node role's equivalent.
type nodeSources struct {
	beat      *health.Heartbeat
	scan      func() (nodescan.Counters, int, bool)
	profiling func() nodescan.ProfilingCoverage
	shipped   func() (uint64, uint64)
}

func (n nodeSources) gather() ([]*dto.MetricFamily, error) {
	m := metrics.Node{
		Process: metrics.Process{
			Role:      metrics.RoleNode,
			Version:   version,
			PassStart: n.beat.Last(),
			Deadline:  n.beat.Deadline(),
		},
		Profiling: n.profiling(),
	}
	// Before the first pass finishes there is nothing to say about a scan, and
	// zeroes would read as a node that walked an empty process table.
	if counters, inScope, ok := n.scan(); ok {
		m.Scanned, m.Scan, m.PodsInScope = true, counters, inScope
	}
	m.ReportsShipped, m.ReportFailures = n.shipped()
	return metrics.CollectNode(m).Families()
}
