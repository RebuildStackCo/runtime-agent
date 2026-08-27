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

// traceWithSourceFile builds a trace whose frames carry a given source path,
// the way the compiler recorded it in the binary.
func traceWithSourceFile(fn, sourceFile string) *libpf.Trace {
	tr := &libpf.Trace{}
	f := libpf.Frame{
		FunctionName: libpf.Intern(fn),
		SourceFile:   libpf.Intern(sourceFile),
		SourceLine:   412,
	}
	tr.Frames.Append(&f)
	return tr
}

// TestTheBuildMachinesPathNeverEntersTheBuffer pins ADR 0041: a Go binary built
// without -trimpath records absolute paths from whatever machine compiled it,
// and those reached the shipped profile verbatim — CI workspace, internal VCS
// host, source-tree layout, none of it the customer's code structure.
//
// The assertion is on the buffer rather than the wire, because the point is that
// the path is cut at the seam and the agent never holds it.
func TestTheBuildMachinesPathNeverEntersTheBuffer(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{
			name: "a CI absolute path",
			path: "/home/jenkins/workspace/acme-payments/src/git.acme-internal.example/payments/api/server.go",
			want: "server.go",
		},
		{
			name: "a trimpath-style module path",
			path: "github.com/acme/app/api/server.go",
			want: "server.go",
		},
		{
			name: "a Windows build path",
			path: `C:\build\acme\internal\billing\ledger.go`,
			want: "ledger.go",
		},
		{
			name: "already a base name",
			path: "hot.go",
			want: "hot.go",
		},
		{
			name: "no source file at all",
			path: "",
			want: "",
		},
		{
			name: "a path that is nothing but a separator",
			path: "/",
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := &Buffer{}
			rep := newTraceReporter(buf)
			trace := traceWithSourceFile("main.(*Server).handleCharge", c.path)
			if err := rep.ReportTraceEvent(trace, &samples.TraceEventMeta{Value: 1, PID: 7}); err != nil {
				t.Fatal(err)
			}

			got := buf.Samples()[0].Frames[0]
			if got.File != c.want {
				t.Errorf("File = %q, want %q", got.File, c.want)
			}
			if strings.ContainsAny(got.File, `/\`) {
				t.Errorf("File %q still carries a directory; the build machine's path must not reach the buffer", got.File)
			}
			// The line number stays: it is a bare integer and says nothing
			// about the machine that compiled the binary.
			if got.Line != 412 {
				t.Errorf("Line = %d, want 412", got.Line)
			}
		})
	}
}
