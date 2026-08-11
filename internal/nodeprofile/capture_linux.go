//go:build linux

package nodeprofile

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/ebpf-profiler/libpf"
	"go.opentelemetry.io/ebpf-profiler/times"
	"go.opentelemetry.io/ebpf-profiler/tracer"
)

// clockSyncInterval matches the upstream default for periodic realtime-clock
// synchronization.
const clockSyncInterval = 3 * time.Minute

// startCapture loads and attaches the eBPF CPU profiler and pumps symbolized
// traces into our reporter until ctx is cancelled. It replicates the minimal
// CPU-only orchestration that the upstream internal/controller performs over the
// public tracer API (that controller is internal, so we cannot call it): load,
// attach the perf-event tracer, enable profiling, attach the scheduler monitor,
// start the map monitors, and hand each raw trace to HandleTrace, which
// symbolizes it and calls our ReportTraceEvent.
//
// Any load/attach failure wraps ErrUnsupported so the caller degrades to
// scanner-only rather than failing the node (ADR 0011 §2; constraint b — the
// version gate passing does not guarantee the programs load).
func startCapture(ctx context.Context, logger *slog.Logger, cfg Config, rep *traceReporter, buf *Buffer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	intervals := times.New(cfg.ReporterInterval, cfg.MonitorInterval, cfg.MonitorInterval)
	times.StartRealtimeSync(ctx, clockSyncInterval)

	trc, err := tracer.NewTracer(ctx, &tracer.Config{
		TraceReporter:     rep,
		Intervals:         intervals,
		SamplesPerSecond:  cfg.SamplesPerSecond,
		FilterErrorFrames: true,
		FilterIdleFrames:  true,
		// The node already ran the kernel/BTF readiness gate (ebpfgate,
		// ADR 0011 §2), so skip the tracer's own version check.
		KernelVersionCheck: false,
	})
	if err != nil {
		return fmt.Errorf("%w: load eBPF tracer: %w", ErrUnsupported, err)
	}
	defer trc.Close()

	trc.StartPIDEventProcessor(ctx)
	if err := trc.AttachTracer(nil); err != nil {
		return fmt.Errorf("%w: attach tracer: %w", ErrUnsupported, err)
	}
	if err := trc.EnableProfiling(); err != nil {
		return fmt.Errorf("%w: enable profiling: %w", ErrUnsupported, err)
	}
	if err := trc.AttachSchedMonitor(); err != nil {
		return fmt.Errorf("%w: attach scheduler monitor: %w", ErrUnsupported, err)
	}
	// A missing prctl monitor only delays process-context discovery; core
	// profiling is unaffected, so warn and continue.
	if err := trc.AttachPrctlMonitor(); err != nil {
		logger.Warn("ebpf prctl monitor not attached; process-context discovery delayed", "error", err)
	}

	traceCh := make(chan *libpf.EbpfTrace)
	if err := trc.StartMapMonitors(ctx, traceCh); err != nil {
		return fmt.Errorf("%w: start map monitors: %w", ErrUnsupported, err)
	}
	logger.Info("ebpf capture started", "samples_per_second", cfg.SamplesPerSecond)

	progress := time.NewTicker(cfg.ReporterInterval)
	defer progress.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("ebpf capture stopping")
			return nil
		case trace := <-traceCh:
			if trace != nil {
				// HandleTrace symbolizes and calls rep.ReportTraceEvent.
				trc.HandleTrace(trace)
			}
		case <-trc.Done():
			return fmt.Errorf("%w: tracer stopped unexpectedly", ErrUnsupported)
		case <-progress.C:
			logProgress(logger, buf)
		}
	}
}
