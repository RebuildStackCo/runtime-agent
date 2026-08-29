package rollup

import (
	"testing"
	"time"
)

var netStart = time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

func netKey() NetworkKey {
	return NetworkKey{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web"}
}

// The property the backend depends on: a delta spanning a boundary is divided
// between the windows it covers, and the parts sum to exactly what was observed
// (ADR 0006).
func TestNetworkDeltaIsSplitAcrossWindowsExactly(t *testing.T) {
	acc := NewNetworkAccumulator(time.Hour)
	from := netStart.Add(45 * time.Minute)
	to := netStart.Add(75 * time.Minute)
	acc.Observe(netKey(), from, to, NetworkDelta{RxBytes: 1000, TxBytes: 999, Interfaces: 1}, false)

	records := acc.CloseBefore(netStart.Add(3 * time.Hour))
	if len(records) != 2 {
		t.Fatalf("records = %d, want one per window", len(records))
	}
	var rx, tx int64
	for _, r := range records {
		rx += r.RxBytes
		tx += r.TxBytes
	}
	if rx != 1000 || tx != 999 {
		t.Errorf("rx/tx = %d/%d, want 1000/999 — the split must not lose or invent bytes", rx, tx)
	}
	// Half the interval on each side, and the odd byte lands somewhere rather
	// than nowhere.
	if records[0].RxBytes != 500 || records[1].RxBytes != 500 {
		t.Errorf("rx per window = %d/%d, want 500/500", records[0].RxBytes, records[1].RxBytes)
	}
}

// A sample is an event, not a quantity: it is counted once even when the
// interval it describes spans two windows.
func TestASampleIsCountedOnce(t *testing.T) {
	acc := NewNetworkAccumulator(time.Hour)
	acc.Observe(netKey(), netStart.Add(45*time.Minute), netStart.Add(75*time.Minute),
		NetworkDelta{RxBytes: 10, Interfaces: 1}, false)

	records := acc.CloseBefore(netStart.Add(3 * time.Hour))
	var samples int64
	for _, r := range records {
		samples += r.Samples
	}
	if samples != 1 {
		t.Errorf("samples = %d across the two windows, want 1", samples)
	}
}

// Zero bytes with a sample behind them is a quiet workload; zero bytes with no
// sample is a cluster whose runtime does not report the counters. The payload
// has to keep them apart (ADR 0013).
func TestSamplesSeparateQuietFromUnobserved(t *testing.T) {
	acc := NewNetworkAccumulator(time.Hour)
	acc.Observe(netKey(), netStart.Add(time.Minute), netStart.Add(2*time.Minute),
		NetworkDelta{Interfaces: 1}, false)

	records := acc.CloseBefore(netStart.Add(3 * time.Hour))
	if len(records) != 1 {
		t.Fatalf("records = %d, want one", len(records))
	}
	if records[0].RxBytes != 0 || records[0].Samples != 1 {
		t.Errorf("rx/samples = %d/%d, want 0/1 — observed and quiet", records[0].RxBytes, records[0].Samples)
	}
}

func TestNetworkRecordsMerge(t *testing.T) {
	acc := NewNetworkAccumulator(time.Hour)
	acc.Observe(netKey(), netStart.Add(time.Minute), netStart.Add(2*time.Minute),
		NetworkDelta{RxBytes: 100, TxBytes: 200, RxErrors: 1, Interfaces: 1}, false)
	a := acc.CloseBefore(netStart.Add(3 * time.Hour))[0]

	acc2 := NewNetworkAccumulator(time.Hour)
	acc2.Observe(netKey(), netStart.Add(3*time.Minute), netStart.Add(4*time.Minute),
		NetworkDelta{RxBytes: 50, TxBytes: 25, TxErrors: 2, Interfaces: 3}, true)
	b := acc2.CloseBefore(netStart.Add(3 * time.Hour))[0]

	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if a.RxBytes != 150 || a.TxBytes != 225 || a.RxErrors != 1 || a.TxErrors != 2 || a.Samples != 2 {
		t.Errorf("merged = %+v", a)
	}
	if a.Interfaces != 3 {
		t.Errorf("interfaces = %d, want the larger of the two", a.Interfaces)
	}
	// Host-network is sticky: it says these counters describe a machine, and
	// merging cannot make that untrue.
	if !a.HostNetwork {
		t.Error("host_network was lost in the merge")
	}
}

func TestMergingDifferentIdentitiesFails(t *testing.T) {
	acc := NewNetworkAccumulator(time.Hour)
	acc.Observe(netKey(), netStart.Add(time.Minute), netStart.Add(2*time.Minute),
		NetworkDelta{RxBytes: 1, Interfaces: 1}, false)
	acc.Observe(NetworkKey{Namespace: "other", WorkloadKind: "Deployment", WorkloadName: "web"},
		netStart.Add(time.Minute), netStart.Add(2*time.Minute),
		NetworkDelta{RxBytes: 1, Interfaces: 1}, false)
	records := acc.CloseBefore(netStart.Add(3 * time.Hour))
	if err := records[0].Merge(records[1]); err == nil {
		t.Error("merging two workloads' records succeeded")
	}
}
