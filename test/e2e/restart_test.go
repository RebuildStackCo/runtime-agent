//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// TestARestartResumesTheOpenWindowFromTheSpool runs the usage pipeline against
// kind, stops it mid-window the way an OOM kill or a drain would, and starts a
// second one over the same spool. The assertion is on the spool, not on logs:
// the window's file must still hold what the first process measured, plus what
// the second one added (ADR 0072).
func TestARestartResumesTheOpenWindowFromTheSpool(t *testing.T) {
	clientset := clusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ns := startBurner(ctx, t, clientset, "restart")
	spoolDir := t.TempDir()

	first, stop := context.WithCancel(ctx)
	_, _, wait := startUsagePipeline(first, t, clientset, spoolDir)
	before := waitForBurnerSnapshot(ctx, t, spoolDir, ns, func(r *rollup.Record) bool {
		return r.CPU.CoreNanoseconds > 3e9 && r.CPU.Samples > 0
	})
	t.Logf("before the restart: %d core ns over %d samples, window %s",
		before.CPU.CoreNanoseconds, before.CPU.Samples, before.WindowStart)

	// Waiting matters: the first process must be gone before the second reads
	// the spool, or the two would race for the window's file.
	stop()
	wait()

	_, _, _ = startUsagePipeline(ctx, t, clientset, spoolDir)
	after := waitForBurnerSnapshot(ctx, t, spoolDir, ns, func(r *rollup.Record) bool {
		return r.WindowStart.Equal(before.WindowStart) && r.CPU.Samples > before.CPU.Samples
	})

	// Sample counts only ever rise inside a window, so a snapshot written after
	// the restart carrying more of them than the one before it can only have
	// been built on top of what the spool held.
	if after.CPU.CoreNanoseconds < before.CPU.CoreNanoseconds {
		t.Errorf("core nanoseconds fell from %d to %d across a restart: the window was reopened empty",
			before.CPU.CoreNanoseconds, after.CPU.CoreNanoseconds)
	}
	if after.CPU.CoveredNanoseconds < before.CPU.CoveredNanoseconds {
		t.Errorf("covered nanoseconds fell from %d to %d across a restart",
			before.CPU.CoveredNanoseconds, after.CPU.CoveredNanoseconds)
	}
	if after.Memory.Samples < before.Memory.Samples {
		t.Errorf("memory samples fell from %d to %d across a restart",
			before.Memory.Samples, after.Memory.Samples)
	}
	t.Logf("after the restart: %d core ns over %d samples", after.CPU.CoreNanoseconds, after.CPU.Samples)
}

// waitForBurnerSnapshot polls the spool until some open-window snapshot holds a
// burner record satisfying want, and returns it.
func waitForBurnerSnapshot(ctx context.Context, t *testing.T, spoolDir, ns string,
	want func(*rollup.Record) bool) *rollup.Record {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	var last *rollup.Record
	for time.Now().Before(deadline) && ctx.Err() == nil {
		for _, r := range burnerSnapshotRecords(t, spoolDir, ns) {
			last = r
			if want(r) {
				return r
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no burner snapshot matched before the deadline; last seen: %+v", last)
	return nil
}

// burnerSnapshotRecords reads every open-window snapshot in the spool and
// returns the burner's records from them.
func burnerSnapshotRecords(t *testing.T, spoolDir, ns string) []*rollup.Record {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(spoolDir, "usage-*.snapshot.json"))
	if err != nil {
		t.Fatalf("listing spool: %v", err)
	}
	var out []*rollup.Record
	for _, path := range matches {
		raw, err := os.ReadFile(path) // #nosec G304 -- test-controlled dir
		if err != nil {
			continue // a rename raced us; the next pass sees the published file
		}
		var payload struct {
			Records []*rollup.Record `json:"records"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			continue
		}
		for _, r := range payload.Records {
			if r.Namespace == ns && r.WorkloadName == "burner" {
				out = append(out, r)
			}
		}
	}
	return out
}
