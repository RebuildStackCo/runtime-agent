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

// startCapture loads and attaches the eBPF CPU profiler and returns a run
// function that pumps symbolized traces into the reporter until ctx is done.
// Setup (load/attach) is synchronous so its errors — a graceful refusal, since
// the version gate passing does not guarantee the programs load (LSM, lockdown,
// perf_event_paranoid) — surface to the caller wrapping ErrUnsupported. It
// replicates the minimal CPU-only orchestration the upstream internal/controller
// performs over the public tracer API.
func startCapture(ctx context.Context, logger *slog.Logger, cfg Config, rep *traceReporter) (func(), error) {
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
		return nil, fmt.Errorf("%w: load eBPF tracer: %w", ErrUnsupported, err)
	}

	trc.StartPIDEventProcessor(ctx)
	if err := trc.AttachTracer(nil); err != nil {
		trc.Close()
		return nil, fmt.Errorf("%w: attach tracer: %w", ErrUnsupported, err)
	}
	if err := trc.EnableProfiling(); err != nil {
		trc.Close()
		return nil, fmt.Errorf("%w: enable profiling: %w", ErrUnsupported, err)
	}
	if err := trc.AttachSchedMonitor(); err != nil {
		trc.Close()
		return nil, fmt.Errorf("%w: attach scheduler monitor: %w", ErrUnsupported, err)
	}
	// A missing prctl monitor only delays process-context discovery; core
	// profiling is unaffected, so warn and continue.
	if err := trc.AttachPrctlMonitor(); err != nil {
		logger.Warn("ebpf prctl monitor not attached; process-context discovery delayed", "error", err)
	}

	traceCh := make(chan *libpf.EbpfTrace)
	if err := trc.StartMapMonitors(ctx, traceCh); err != nil {
		trc.Close()
		return nil, fmt.Errorf("%w: start map monitors: %w", ErrUnsupported, err)
	}
	logger.Info("ebpf capture started", "samples_per_second", cfg.SamplesPerSecond)

	run := func() {
		defer trc.Close()
		for {
			select {
			case <-ctx.Done():
				logger.Info("ebpf capture stopping")
				return
			case trace := <-traceCh:
				if trace != nil {
					// HandleTrace symbolizes and calls rep.ReportTraceEvent.
					trc.HandleTrace(trace)
				}
			case <-trc.Done():
				logger.Error("ebpf tracer stopped unexpectedly")
				return
			}
		}
	}
	return run, nil
}
