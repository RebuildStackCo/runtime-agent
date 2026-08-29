package rollup

import (
	"fmt"
	"sort"
	"time"
)

// NetworkKey identifies the aggregation subject for network counters: one
// workload, not one container.
//
// It is a different key from Key on purpose. The counters are the pod's — every
// container in a pod shares one network namespace — so there is no container to
// attribute them to, and nesting them on container records would break the one
// documented use of those records: that summing them gives the pod's total
// (backend-requirements.md §4, ADR 0053).
type NetworkKey struct {
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workload_kind"`
	WorkloadName string `json:"workload_name"`
}

// NetworkRecord is one window of one workload's network counters, summed across
// its pods. Bytes and errors are exact cumulative deltas, split pro rata where a
// delta spans a window boundary — the same treatment CPU time gets (ADR 0006).
type NetworkRecord struct {
	NetworkKey
	WindowStart   time.Time `json:"window_start"`
	WindowSeconds int64     `json:"window_seconds"`

	RxBytes  int64 `json:"rx_bytes"`
	TxBytes  int64 `json:"tx_bytes"`
	RxErrors int64 `json:"rx_errors"`
	TxErrors int64 `json:"tx_errors"`

	// Samples is how many pod observations carried network counters at all.
	// Zero makes the byte counts mean "never observed" — this cluster's runtime
	// or CNI does not report them — where non-zero makes the same zero mean
	// "observed, and it moved nothing" (ADR 0013).
	Samples int64 `json:"samples"`
	// CoveredNanoseconds is how much of the window those observations span, the
	// denominator for any rate derived from the bytes above.
	CoveredNanoseconds int64 `json:"covered_nanoseconds"`
	// Interfaces is the most interfaces seen on any one pod. Their names are not
	// collected: a name is cluster network topology, and the count is what says
	// a second interface exists at all.
	Interfaces int `json:"interfaces,omitempty"`
	// HostNetwork marks a workload whose pods share the node's network
	// namespace. The kubelet then reports the *node's* interfaces, so these
	// counters describe the machine and not the workload — summing them with
	// the rest attributes the whole cluster's traffic to one DaemonSet
	// (ADR 0053 §2).
	HostNetwork bool `json:"host_network,omitempty"`
}

// NetworkAccumulator assigns network counter deltas to wall-clock-aligned
// windows. Like Accumulator it is owned by the poller's goroutine and is not
// safe for concurrent use.
type NetworkAccumulator struct {
	win  windowing
	open map[networkOpenKey]*NetworkRecord
}

type networkOpenKey struct {
	NetworkKey
	start int64 // window start, unix nanoseconds
}

// NewNetworkAccumulator returns an accumulator over windows of the given
// length, which must be positive.
func NewNetworkAccumulator(windowLength time.Duration) *NetworkAccumulator {
	if windowLength <= 0 {
		panic("rollup: window length must be positive")
	}
	return &NetworkAccumulator{
		win:  windowing{length: windowLength},
		open: map[networkOpenKey]*NetworkRecord{},
	}
}

func (a *NetworkAccumulator) recordAt(k NetworkKey, start time.Time) *NetworkRecord {
	ok := networkOpenKey{NetworkKey: k, start: start.UnixNano()}
	r := a.open[ok]
	if r == nil {
		r = &NetworkRecord{
			NetworkKey:    k,
			WindowStart:   start,
			WindowSeconds: a.win.seconds(),
		}
		a.open[ok] = r
	}
	return r
}

// NetworkDelta is one pod's counter movement over [from, to).
type NetworkDelta struct {
	RxBytes    int64
	TxBytes    int64
	RxErrors   int64
	TxErrors   int64
	Interfaces int
}

// Observe records one pod's counter deltas over [from, to), splitting each
// across the windows the interval covers. The sample and the covered time are
// split the same way, so a window's coverage always describes that window.
func (a *NetworkAccumulator) Observe(k NetworkKey, from, to time.Time, d NetworkDelta, hostNetwork bool) {
	if !to.After(from) {
		return
	}
	a.win.split(from, to, d.RxBytes, func(start time.Time, part int64) {
		a.recordAt(k, start).RxBytes += part
	})
	a.win.split(from, to, d.TxBytes, func(start time.Time, part int64) {
		a.recordAt(k, start).TxBytes += part
	})
	a.win.split(from, to, d.RxErrors, func(start time.Time, part int64) {
		a.recordAt(k, start).RxErrors += part
	})
	a.win.split(from, to, d.TxErrors, func(start time.Time, part int64) {
		a.recordAt(k, start).TxErrors += part
	})
	a.win.split(from, to, to.Sub(from).Nanoseconds(), func(start time.Time, part int64) {
		r := a.recordAt(k, start)
		r.CoveredNanoseconds += part
		r.Interfaces = max(r.Interfaces, d.Interfaces)
		r.HostNetwork = r.HostNetwork || hostNetwork
	})
	// One observation, counted once, in the window the interval ends in: a
	// sample is an event, not a quantity to divide.
	a.recordAt(k, a.win.startOf(to)).Samples++
}

// CloseBefore removes and returns every record whose window ended at or before
// now, sorted deterministically.
func (a *NetworkAccumulator) CloseBefore(now time.Time) []*NetworkRecord {
	var out []*NetworkRecord
	for ok, r := range a.open {
		if !r.WindowStart.Add(a.win.length).After(now) {
			out = append(out, r)
			delete(a.open, ok)
		}
	}
	sortNetworkRecords(out)
	return out
}

// Snapshots returns copies of all open records, sorted deterministically.
func (a *NetworkAccumulator) Snapshots() []*NetworkRecord {
	out := make([]*NetworkRecord, 0, len(a.open))
	for _, r := range a.open {
		c := *r
		out = append(out, &c)
	}
	sortNetworkRecords(out)
	return out
}

// Merge folds o into r. Both must describe the same key and window; totals add,
// the interface count takes the larger, and host-network is sticky. It is what
// makes a short window fold into a longer one losslessly (ADR 0006).
func (r *NetworkRecord) Merge(o *NetworkRecord) error {
	if r.NetworkKey != o.NetworkKey || !r.WindowStart.Equal(o.WindowStart) || r.WindowSeconds != o.WindowSeconds {
		return fmt.Errorf("rollup: merging network records with different identities")
	}
	r.RxBytes += o.RxBytes
	r.TxBytes += o.TxBytes
	r.RxErrors += o.RxErrors
	r.TxErrors += o.TxErrors
	r.Samples += o.Samples
	r.CoveredNanoseconds += o.CoveredNanoseconds
	r.Interfaces = max(r.Interfaces, o.Interfaces)
	r.HostNetwork = r.HostNetwork || o.HostNetwork
	return nil
}

func sortNetworkRecords(rs []*NetworkRecord) {
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if !a.WindowStart.Equal(b.WindowStart) {
			return a.WindowStart.Before(b.WindowStart)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.WorkloadKind != b.WorkloadKind {
			return a.WorkloadKind < b.WorkloadKind
		}
		return a.WorkloadName < b.WorkloadName
	})
}
