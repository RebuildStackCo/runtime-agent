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

// Config holds the capture parameters. The profiler samples system-wide and
// this type carries only the sampling rate: which containers a window's samples
// are kept for, and every ceiling around that (top-N, duration, interval,
// overhead), are applied by the pipeline that drains the session, not here
// (ADR 0011 §3).
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

// Session is a running capture. The pipeline drains it once per window; the
// profiler keeps accumulating in the background until ctx (passed to Start) is
// cancelled.
type Session struct {
	buf *Buffer
}

// Start loads and attaches the eBPF profiler and begins accumulating symbolized
// samples in the background, returning a Session to drain. Setup failures
// (non-Linux build, or the programs failing to load/attach) are returned
// synchronously as an error wrapping ErrUnsupported, so the caller degrades to
// scanner-only. Before the allow-list filter, the captured profile exists
// nowhere but the Session's Buffer.
func Start(ctx context.Context, logger *slog.Logger, cfg Config) (*Session, error) {
	buf := &Buffer{}
	rep := newTraceReporter(buf)
	run, err := startCapture(ctx, logger, cfg.withDefaults(), rep)
	if err != nil {
		return nil, err
	}
	go run()
	return &Session{buf: buf}, nil
}

// Drain cuts the current window: it returns the samples accumulated since the
// last Drain and resets the buffer.
func (s *Session) Drain() []Sample { return s.buf.Drain() }

// logProgress emits an aggregate-only progress line. It is the single place the
// capture path logs anything about a running profile, and it logs counts, never
// a function name or file (constraint a). Kept here (not in the Linux-only
// driver) so it is covered by a cross-platform test.
func logProgress(logger *slog.Logger, buf *Buffer) {
	samples, frames := buf.Counters()
	logger.Info("ebpf capture progress", "samples", samples, "frames", frames)
}
