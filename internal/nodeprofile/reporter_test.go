package nodeprofile

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/ebpf-profiler/libpf"
	"go.opentelemetry.io/ebpf-profiler/reporter/samples"
)

// makeTrace builds a synthetic symbolized trace, as the tracer would hand us
// after HandleTrace.
func makeTrace(fns ...string) *libpf.Trace {
	tr := &libpf.Trace{}
	for _, fn := range fns {
		f := libpf.Frame{
			FunctionName: libpf.Intern(fn),
			SourceFile:   libpf.Intern("hot.go"),
			SourceLine:   42,
		}
		tr.Frames.Append(&f)
	}
	return tr
}

func TestReporterAccumulates(t *testing.T) {
	buf := &Buffer{}
	rep := newTraceReporter(buf)

	if err := rep.ReportTraceEvent(makeTrace("main.hot", "main.mix"), &samples.TraceEventMeta{Value: 3, PID: 123}); err != nil {
		t.Fatal(err)
	}
	if err := rep.ReportTraceEvent(makeTrace("main.hot"), &samples.TraceEventMeta{Value: 1, PID: 123}); err != nil {
		t.Fatal(err)
	}

	s, f := buf.Counters()
	if s != 2 || f != 3 {
		t.Errorf("counters = %d samples, %d frames; want 2, 3", s, f)
	}
	got := buf.Samples()
	if len(got) != 2 || len(got[0].Frames) != 2 || got[0].Frames[0].Function != "main.hot" {
		t.Fatalf("samples not accumulated as expected: %+v", got)
	}
	if got[0].Frames[0].File != "hot.go" || got[0].Frames[0].Line != 42 {
		t.Errorf("frame fields not copied: %+v", got[0].Frames[0])
	}
}

// TestProgressLogsCountsNotSymbols is the constraint-(a) guard: the only line the
// capture path logs about a running profile carries aggregate counts and never a
// function name or file, even though the buffer holds them in memory.
func TestProgressLogsCountsNotSymbols(t *testing.T) {
	buf := &Buffer{}
	if err := newTraceReporter(buf).ReportTraceEvent(
		makeTrace("main.secretHotFunction"),
		&samples.TraceEventMeta{Value: 1},
	); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	logProgress(slog.New(slog.NewTextHandler(&out, nil)), buf)

	s := out.String()
	if !strings.Contains(s, "samples=1") || !strings.Contains(s, "frames=1") {
		t.Errorf("progress line missing counts: %s", s)
	}
	if strings.Contains(s, "secretHotFunction") || strings.Contains(s, "hot.go") {
		t.Errorf("progress line leaked a symbol: %s", s)
	}
}
