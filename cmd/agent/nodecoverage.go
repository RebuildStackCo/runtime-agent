package main

import (
	"sync"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeprofile"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// stateDisabled is the profiling state of a node whose master switch is off:
// not a refusal, and the difference matters to whoever is asking why a cluster
// has no profiles (ADR 0060 §2).
const stateDisabled = "disabled"

// profilingMetrics accumulates what this node's profiler did, for the report
// the scanner ships. The gate writes it at startup, the capture pipeline writes
// it every window, and the scan pass reads it — three goroutines, so the
// counters are behind a mutex.
//
// Cumulative since the process started, because the controller keeps the latest
// report per node rather than summing a sequence: a lost report then costs
// staleness and never a count (ADR 0060 §3).
type profilingMetrics struct {
	mu  sync.Mutex
	cov nodescan.ProfilingCoverage
}

func newProfilingMetrics() *profilingMetrics {
	return &profilingMetrics{cov: nodescan.ProfilingCoverage{State: stateDisabled}}
}

// setState records why this node is or is not profiling. It is called at most
// twice: once by the gate, and once more if the eBPF program then fails to load.
func (m *profilingMetrics) setState(state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cov.State = state
}

func (m *profilingMetrics) window() { m.add(func(c *nodescan.ProfilingCoverage) { c.Windows++ }) }
func (m *profilingMetrics) noScope() {
	m.add(func(c *nodescan.ProfilingCoverage) { c.WindowsNoScope++ })
}
func (m *profilingMetrics) noTargets() {
	m.add(func(c *nodescan.ProfilingCoverage) { c.WindowsNoTargets++ })
}
func (m *profilingMetrics) noSamples() {
	m.add(func(c *nodescan.ProfilingCoverage) { c.WindowsNoSamples++ })
}
func (m *profilingMetrics) shipped() {
	m.add(func(c *nodescan.ProfilingCoverage) { c.ProfilesShipped++ })
}
func (m *profilingMetrics) invalid() {
	m.add(func(c *nodescan.ProfilingCoverage) { c.ProfilesInvalid++ })
}
func (m *profilingMetrics) unshipped() {
	m.add(func(c *nodescan.ProfilingCoverage) { c.ProfilesUnshipped++ })
}

func (m *profilingMetrics) outOfScope(samples int) {
	m.add(func(c *nodescan.ProfilingCoverage) { c.SamplesOutOfScope += samples })
}

// redacted folds in what the symbol filter dropped building one profile. The
// counters carry no identity of what was redacted (CLAUDE.md invariant 6).
func (m *profilingMetrics) redacted(f nodeprofile.FilterCounters) {
	m.add(func(c *nodescan.ProfilingCoverage) {
		c.ThirdPartyDropped += f.ThirdPartyDropped
		c.UnsymbolizedDropped += f.UnsymbolizedDropped
		c.SamplesFiltered += f.SamplesFiltered
	})
}

func (m *profilingMetrics) add(f func(*nodescan.ProfilingCoverage)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&m.cov)
}

// snapshot is what the scanner's report carries.
func (m *profilingMetrics) snapshot() nodescan.ProfilingCoverage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cov
}
