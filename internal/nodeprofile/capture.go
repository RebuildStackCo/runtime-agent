package nodeprofile

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// ErrUnsupported means the eBPF profiler cannot run here: either this is a
// non-Linux build, or the eBPF programs failed to load or attach at runtime
// (LSM, kernel lockdown, perf_event_paranoid — the kernel-version gate can pass
// and this still fail). The caller treats it as a graceful refusal
// (ebpfgate.ReasonProgramLoadFailed), never a fatal error (ADR 0011 §2).
var ErrUnsupported = errors.New("nodeprofile: eBPF profiler unavailable")

// Config holds the capture parameters. In this slice the profiler runs
// system-wide; the eligible set and ceilings (which workloads, capture
// duration, frequency, overhead) arrive from the node ConfigMap in a later
// slice (ADR 0011 §3).
type Config struct {
	SamplesPerSecond int
	ReporterInterval time.Duration // also the progress-log cadence
	MonitorInterval  time.Duration
}

func (c Config) withDefaults() Config {
	if c.SamplesPerSecond <= 0 {
		c.SamplesPerSecond = 20
	}
	if c.ReporterInterval <= 0 {
		c.ReporterInterval = 5 * time.Second
	}
	if c.MonitorInterval <= 0 {
		c.MonitorInterval = 5 * time.Second
	}
	return c
}

// Run starts the eBPF CPU profiler, accumulating symbolized samples in an
// in-memory Buffer and periodically logging aggregate counters only. It blocks
// until ctx is cancelled (returning nil) or returns an error wrapping
// ErrUnsupported when the profiler cannot run. It never logs symbol-bearing
// data: before the allow-list filter (a later slice), the captured profile
// exists nowhere but the Buffer this function owns.
func Run(ctx context.Context, logger *slog.Logger, cfg Config) error {
	buf := &Buffer{}
	rep := newTraceReporter(buf)
	return startCapture(ctx, logger, cfg.withDefaults(), rep, buf)
}

// logProgress emits an aggregate-only progress line. It is the single place the
// capture path logs anything about a running profile, and it logs counts, never
// a function name or file (constraint a). Kept here (not in the Linux-only
// driver) so it is covered by a cross-platform test.
func logProgress(logger *slog.Logger, buf *Buffer) {
	samples, frames := buf.Counters()
	logger.Info("ebpf capture progress", "samples", samples, "frames", frames)
}
