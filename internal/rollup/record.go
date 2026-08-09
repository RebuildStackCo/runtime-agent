package rollup

import (
	"fmt"
	"time"
)

// Key identifies the aggregation subject: all replicas of one container of
// one workload merge into a single record (ADR 0006). Identities of
// filtered-out pods never reach a Key — samples are filtered before
// accumulation.
type Key struct {
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workload_kind"`
	WorkloadName string `json:"workload_name"`
	Container    string `json:"container"`
}

// CPUUsage is the CPU side of a record. The totals are exact cumulative
// counter deltas; the distribution fields describe sampled rates, each rate
// being one counter delta averaged over its poll interval, in millicores.
type CPUUsage struct {
	// CoreNanoseconds is the exact CPU time consumed inside the window,
	// split pro rata where a counter delta spans a window boundary.
	CoreNanoseconds int64 `json:"core_nanoseconds"`
	// ThrottledPeriods and TotalPeriods are CFS throttling counter deltas.
	// Zero when the cluster does not expose them — the active signal set in
	// the agent's self-info says which reading applies.
	ThrottledPeriods int64 `json:"throttled_periods"`
	TotalPeriods     int64 `json:"total_periods"`
	// PSIStallNanoseconds is PSI some-CPU stall time, where exposed.
	PSIStallNanoseconds int64 `json:"psi_stall_nanoseconds"`

	Samples  int64      `json:"samples"`
	MinMilli int64      `json:"min_milli"`
	MaxMilli int64      `json:"max_milli"`
	Hist     *Histogram `json:"hist_milli"`
}

// observeRate records one sampled rate in millicores.
func (u *CPUUsage) observeRate(milli int64) {
	if u.Samples == 0 || milli < u.MinMilli {
		u.MinMilli = milli
	}
	if u.Samples == 0 || milli > u.MaxMilli {
		u.MaxMilli = milli
	}
	u.Samples++
	u.Hist.Observe(milli)
}

// MemoryUsage is the memory side of a record: the working-set gauge,
// sampled. True inter-sample peaks are unobservable on cgroup v2; peak risk
// is carried by OOM events and PSI instead (ADR 0006).
type MemoryUsage struct {
	Samples  int64 `json:"samples"`
	MinBytes int64 `json:"min_bytes"`
	MaxBytes int64 `json:"max_bytes"`
	// SumBytes exists so the backend can derive the exact window average.
	SumBytes int64 `json:"sum_bytes"`
	// PSIStallNanoseconds is PSI some-memory stall time, where exposed.
	PSIStallNanoseconds int64 `json:"psi_stall_nanoseconds"`

	Hist *Histogram `json:"hist_bytes"`
}

// observe records one working-set sample in bytes.
func (u *MemoryUsage) observe(bytes int64) {
	if u.Samples == 0 || bytes < u.MinBytes {
		u.MinBytes = bytes
	}
	if u.Samples == 0 || bytes > u.MaxBytes {
		u.MaxBytes = bytes
	}
	u.Samples++
	u.SumBytes += bytes
	u.Hist.Observe(bytes)
}

// Record is one usage rollup: one Key over one wall-clock-aligned window.
// WindowStart and WindowSeconds are part of the natural key — the backend
// merges shorter windows into longer ones losslessly, so the window length
// can change without a schema break.
type Record struct {
	Key
	WindowStart   time.Time `json:"window_start"`
	WindowSeconds int64     `json:"window_seconds"`

	CPU    CPUUsage    `json:"cpu"`
	Memory MemoryUsage `json:"memory"`
}

func newRecord(k Key, start time.Time, windowSeconds int64) *Record {
	return &Record{
		Key:           k,
		WindowStart:   start,
		WindowSeconds: windowSeconds,
		CPU:           CPUUsage{Hist: NewCPUHistogram()},
		Memory:        MemoryUsage{Hist: NewMemoryHistogram()},
	}
}

// Merge folds o into r. Both records must describe the same key and window;
// totals add, extrema combine, histograms merge bucket-wise. Min and max are
// only meaningful while Samples > 0, so an empty side is the identity.
func (r *Record) Merge(o *Record) error {
	if r.Key != o.Key || !r.WindowStart.Equal(o.WindowStart) || r.WindowSeconds != o.WindowSeconds {
		return fmt.Errorf("rollup: merging records with different identities")
	}

	r.CPU.CoreNanoseconds += o.CPU.CoreNanoseconds
	r.CPU.ThrottledPeriods += o.CPU.ThrottledPeriods
	r.CPU.TotalPeriods += o.CPU.TotalPeriods
	r.CPU.PSIStallNanoseconds += o.CPU.PSIStallNanoseconds
	if o.CPU.Samples > 0 {
		if r.CPU.Samples == 0 || o.CPU.MinMilli < r.CPU.MinMilli {
			r.CPU.MinMilli = o.CPU.MinMilli
		}
		if r.CPU.Samples == 0 || o.CPU.MaxMilli > r.CPU.MaxMilli {
			r.CPU.MaxMilli = o.CPU.MaxMilli
		}
		r.CPU.Samples += o.CPU.Samples
	}
	if err := r.CPU.Hist.Merge(o.CPU.Hist); err != nil {
		return err
	}

	r.Memory.PSIStallNanoseconds += o.Memory.PSIStallNanoseconds
	if o.Memory.Samples > 0 {
		if r.Memory.Samples == 0 || o.Memory.MinBytes < r.Memory.MinBytes {
			r.Memory.MinBytes = o.Memory.MinBytes
		}
		if r.Memory.Samples == 0 || o.Memory.MaxBytes > r.Memory.MaxBytes {
			r.Memory.MaxBytes = o.Memory.MaxBytes
		}
		r.Memory.Samples += o.Memory.Samples
		r.Memory.SumBytes += o.Memory.SumBytes
	}
	return r.Memory.Hist.Merge(o.Memory.Hist)
}

func (r *Record) clone() *Record {
	c := *r
	c.CPU.Hist = r.CPU.Hist.clone()
	c.Memory.Hist = r.Memory.Hist.clone()
	return &c
}
