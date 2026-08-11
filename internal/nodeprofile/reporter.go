package nodeprofile

import (
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
				File:     f.SourceFile.String(),
				Line:     line,
				Kind:     f.Type.String(),
			})
		}
	}
	r.buf.Add(s)
	return nil
}
