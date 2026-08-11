//go:build !linux

package nodeprofile

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
)

// startCapture on non-Linux platforms is a stub so the repository builds and
// tests on developer machines (macOS). The eBPF profiler is Linux-only; here it
// is always unavailable, reported as such and treated as a graceful refusal.
func startCapture(_ context.Context, logger *slog.Logger, _ Config, _ *traceReporter, _ *Buffer) error {
	logger.Warn("ebpf capture unavailable on this platform; profiler is Linux-only", "goos", runtime.GOOS)
	return fmt.Errorf("%w: not linux (%s)", ErrUnsupported, runtime.GOOS)
}
