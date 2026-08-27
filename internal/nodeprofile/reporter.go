package nodeprofile

import (
	"strings"

	"go.opentelemetry.io/ebpf-profiler/libpf"
	"go.opentelemetry.io/ebpf-profiler/reporter/samples"
)

// traceReporter implements go.opentelemetry.io/ebpf-profiler/reporter.TraceReporter.
// It is the seam where the profiler hands us a symbolized trace (the tracer's
// HandleTrace resolves symbols before calling this). We copy the frames into our
// own decoupled Sample and accumulate; we log nothing here. The upstream types
// build on all platforms, so this reporter — and its tests — are cross-platform;
// only the tracer that feeds it is Linux-only (see capture_linux.go).
type traceReporter struct {
	buf *Buffer
}

func newTraceReporter(buf *Buffer) *traceReporter { return &traceReporter{buf: buf} }

// ReportTraceEvent accepts one symbolized trace and enqueues it into the buffer.
// It intentionally ignores meta.EnvVars: environment variables routinely carry
// secrets and never leave with a profile (CLAUDE.md invariant 4).
//
// The source file is reduced to its base name here rather than filtered later
// (ADR 0041): without -trimpath the compiler records the build machine's
// absolute path, none of which is the code structure a profile is for. Cutting
// it at the seam means the full path never enters the buffer.
func (r *traceReporter) ReportTraceEvent(trace *libpf.Trace, meta *samples.TraceEventMeta) error {
	s := Sample{
		Value:       meta.Value,
		PID:         int64(meta.PID),
		Comm:        meta.Comm.String(),
		ContainerID: meta.ContainerID.String(),
	}
	if len(trace.Frames) > 0 {
		s.Frames = make([]Frame, 0, len(trace.Frames))
		for i := range trace.Frames {
			f := trace.Frames[i].Value()
			line := int64(f.SourceLine) // #nosec G115 -- source line numbers never exceed int64
			s.Frames = append(s.Frames, Frame{
				Function: f.FunctionName.String(),
				File:     sourceFileBase(f.SourceFile.String()),
				Line:     line,
				Kind:     f.Type.String(),
			})
		}
	}
	r.buf.Add(s)
	return nil
}

// sourceFileBase reduces a compiler-recorded source path to its base name.
//
// Both separators are cut, not just the host's: the path comes from whatever
// machine compiled the binary, which need not be the one running it. An empty
// or separator-only path stays empty rather than becoming a stray separator.
func sourceFileBase(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}
