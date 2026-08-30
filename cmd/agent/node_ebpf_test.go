package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/config"
	"github.com/RebuildStackCo/runtime-agent/internal/ebpfgate"
	"github.com/RebuildStackCo/runtime-agent/internal/nodeprofile"
)

// writeGateFixture builds a proc/sys pair the gate can probe: an osrelease file
// under proc and, optionally, a BTF blob under sys.
func writeGateFixture(t *testing.T, osrelease string, btf bool) (proc, sys string) {
	t.Helper()
	proc, sys = t.TempDir(), t.TempDir()
	d := filepath.Join(proc, "sys", "kernel")
	if err := os.MkdirAll(d, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "osrelease"), []byte(osrelease), 0o600); err != nil {
		t.Fatal(err)
	}
	if btf {
		b := filepath.Join(sys, "kernel", "btf")
		if err := os.MkdirAll(b, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b, "vmlinux"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return proc, sys
}

func TestEBPFGateSupported(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	proc, sys := writeGateFixture(t, "6.8.0-1064-gcp", true)

	m := newEBPFGateMetrics()
	res := ebpfGate(logger, proc, sys, m)

	if !res.Supported() {
		t.Fatalf("Supported() = false, reason %q", res.Reason)
	}
	if m.ready != 1 || len(m.refusals) != 0 {
		t.Errorf("metrics = %+v, want ready=1 no refusals", m)
	}
	if !strings.Contains(buf.String(), "ebpf profile ready") {
		t.Errorf("log missing ready line:\n%s", buf.String())
	}
}

func TestEBPFGateRefusesGracefully(t *testing.T) {
	cases := []struct {
		name      string
		osrelease string
		btf       bool
		reason    ebpfgate.Reason
	}{
		{"old kernel", "5.4.0", true, ebpfgate.ReasonKernelTooOld},
		{"no btf", "6.8.0", false, ebpfgate.ReasonBTFAbsent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			proc, sys := writeGateFixture(t, tc.osrelease, tc.btf)

			m := newEBPFGateMetrics()
			res := ebpfGate(logger, proc, sys, m)

			if res.Supported() {
				t.Fatal("expected a refusal, got supported")
			}
			if m.ready != 0 {
				t.Errorf("ready = %d, want 0", m.ready)
			}
			if m.refusals[tc.reason] != 1 {
				t.Errorf("refusals[%s] = %d, want 1 (all: %+v)", tc.reason, m.refusals[tc.reason], m.refusals)
			}
			if !strings.Contains(buf.String(), "refused") {
				t.Errorf("log missing refusal line:\n%s", buf.String())
			}
		})
	}
}

// TestRunProfilingPipelineGraceful checks the pipeline's graceful path: when the
// profiler is unavailable it records program_load_failed and returns rather than
// blocking or panicking. On Linux this would attempt a real eBPF load (needs a
// privileged BTF host — that path is exercised by the slice-7 e2e), so it is
// skipped there.
func TestRunProfilingPipelineGraceful(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("attempts a real eBPF load on linux; covered by the slice-7 capture e2e")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := newEBPFGateMetrics()
	runProfilingPipeline(context.Background(), logger, config.NodeProfiling{}.Normalized(),
		t.TempDir(), "node", nil, nil, nil, m, &nodeprofile.ModuleIndex{})
	if m.refusals[ebpfgate.ReasonProgramLoadFailed] != 1 {
		t.Errorf("program_load_failed = %d, want 1", m.refusals[ebpfgate.ReasonProgramLoadFailed])
	}
}

// TestRunNodeEBPFRefusalDoesNotStop is the graceful-degradation guard: with the
// master switch on and an unsupported kernel, the node must still run the
// scanner to completion (single pass) and return nil — a refusal degrades, it
// never fails the node or escalates (ADR 0011 §2).
func TestRunNodeEBPFRefusalDoesNotStop(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	proc, sys := writeGateFixture(t, "5.4.0", false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan error, 1)
	go func() {
		done <- runNode(context.Background(), logger,
			[]string{"-proc", proc, "-sys", sys, "-enable-ebpf", "-interval", "0"})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runNode returned error on eBPF refusal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runNode did not return")
	}
}
