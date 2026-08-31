package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeprofile"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// quietLogger keeps a test's expected warnings out of the run's output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// The whole point of ADR 0060: a window that shipped nothing says which of the
// three reasons it was, and each is a different answer to "is this the agent or
// the cluster". Before this they were one early return.
func TestAnEmptyWindowSaysWhichKindOfEmpty(t *testing.T) {
	const podUID = "1234abcd-12ab-34cd-56ef-1234567890ab"
	cid := strings.Repeat("a", 64)
	procRoot := t.TempDir()
	writeCgroup(t, procRoot, 100, podUID, cid)
	samples := []nodeprofile.Sample{{PID: 100, Value: 1, Frames: []nodeprofile.Frame{
		{Function: "main.hot", Kind: "go"},
	}}}

	cases := []struct {
		name    string
		samples []nodeprofile.Sample
		targets map[string]struct{}
		scope   nodescan.Scope
		want    func(nodescan.ProfilingCoverage) int
	}{
		{"no scope", samples, map[string]struct{}{cid: {}}, nodescan.DenyAll(),
			func(c nodescan.ProfilingCoverage) int { return c.WindowsNoScope }},
		{"no targets", samples, nil, nodescan.NewScope([]string{podUID}),
			func(c nodescan.ProfilingCoverage) int { return c.WindowsNoTargets }},
		{"no samples", nil, map[string]struct{}{cid: {}}, nodescan.NewScope([]string{podUID}),
			func(c nodescan.ProfilingCoverage) int { return c.WindowsNoSamples }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newProfilingMetrics()
			processWindow(context.Background(), quietLogger(), staticFilter(nil), 20, procRoot, "node",
				time.Unix(100, 0), time.Unix(160, 0), tc.samples, tc.targets, tc.scope, nil, m)

			got := m.snapshot()
			if got.Windows != 1 {
				t.Errorf("windows = %d, want 1: a window that produced nothing is still a window", got.Windows)
			}
			if n := tc.want(got); n != 1 {
				t.Errorf("the %s counter = %d, want 1 (all: %+v)", tc.name, n, got)
			}
		})
	}
}

// A shipped profile is counted with what the filter dropped building it. The
// drop counts are the signal for an allow-list that stopped matching, which is
// the defect ADR 0059 closed and nothing was watching for (ADR 0060 §2).
func TestAShippedProfileCarriesWhatWasRedacted(t *testing.T) {
	const podUID = "1234abcd-12ab-34cd-56ef-1234567890ab"
	cid := strings.Repeat("a", 64)
	procRoot := t.TempDir()
	writeCgroup(t, procRoot, 100, podUID, cid)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	samples := []nodeprofile.Sample{{PID: 100, Value: 5, Frames: []nodeprofile.Frame{
		{Function: "github.com/vendor/lib.Do", Kind: "go"}, // third-party: redacted
		{Function: "main.hot", Kind: "go"},                 // kept, so the profile validates
	}}}
	m := newProfilingMetrics()
	processWindow(context.Background(), quietLogger(), staticFilter(nil), 20, procRoot, "node",
		time.Unix(100, 0), time.Unix(160, 0), samples, map[string]struct{}{cid: {}},
		nodescan.NewScope([]string{podUID}), newProfileShipper(srv.URL, writeToken(t, "tok")), m)

	got := m.snapshot()
	if got.ProfilesShipped != 1 || got.ProfilesInvalid != 0 || got.ProfilesUnshipped != 0 {
		t.Errorf("shipped/invalid/unshipped = %d/%d/%d, want 1/0/0",
			got.ProfilesShipped, got.ProfilesInvalid, got.ProfilesUnshipped)
	}
	if got.ThirdPartyDropped != 1 || got.SamplesFiltered != 1 {
		t.Errorf("third_party_dropped = %d, samples_filtered = %d, want 1 and 1 (all: %+v)",
			got.ThirdPartyDropped, got.SamplesFiltered, got)
	}
}

// A profile of nothing but the runtime is refused by Validate. It used to leave
// only a log line on the node, which is exactly the silence this closes.
func TestARefusedProfileIsCountedNotJustLogged(t *testing.T) {
	const podUID = "1234abcd-12ab-34cd-56ef-1234567890ab"
	cid := strings.Repeat("a", 64)
	procRoot := t.TempDir()
	writeCgroup(t, procRoot, 100, podUID, cid)

	samples := []nodeprofile.Sample{{PID: 100, Value: 5, Frames: []nodeprofile.Frame{
		{Function: "github.com/vendor/lib.Do", Kind: "go"},
	}}}
	m := newProfilingMetrics()
	processWindow(context.Background(), quietLogger(), staticFilter(nil), 20, procRoot, "node",
		time.Unix(100, 0), time.Unix(160, 0), samples, map[string]struct{}{cid: {}},
		nodescan.NewScope([]string{podUID}), nil, m)

	if got := m.snapshot(); got.ProfilesInvalid != 1 || got.ProfilesShipped != 0 {
		t.Errorf("invalid/shipped = %d/%d, want 1/0 (all: %+v)",
			got.ProfilesInvalid, got.ProfilesShipped, got)
	}
}

// The node reports its state before it reports any profile, because the node
// with the most to explain is the one that will never ship one.
func TestADisabledNodeStillHasAState(t *testing.T) {
	if got := newProfilingMetrics().snapshot().State; got != stateDisabled {
		t.Errorf("state = %q, want %q", got, stateDisabled)
	}
}
