package main

import (
	"slices"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

// A kind written on a fixed cadence is written by a ticker, and the registry
// states that ticker's interval as a protocol constant the backend may rely on
// (ADR 0073 §5). Nothing in the sink can check that: it never sees the ticker.
// This is the other direction — move either number alone and it fails here.
//
// There are two such tickers, both a minute: the periodic pass, which writes
// coverage, the journals and the process counters, and the usage poller's
// snapshot ticker.
func TestFixedCadenceMatchesItsTicker(t *testing.T) {
	tickers := []time.Duration{defaultCoverageInterval, collector.UsageSnapshotInterval}
	for _, entry := range sink.Registry() {
		if entry.Cadence.Trigger != sink.TriggerAlways {
			continue
		}
		if !slices.Contains(tickers, entry.Cadence.Floor) {
			t.Errorf("the registry says %q is written every %s, and no ticker in this agent runs "+
				"at that interval; either the ticker moved and the registry did not, or this "+
				"kind is now written by a third one that belongs in this list",
				entry.Kind, entry.Cadence.Floor)
		}
	}
}
