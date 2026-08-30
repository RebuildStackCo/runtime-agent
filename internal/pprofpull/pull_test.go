package pprofpull

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/pprof/profile"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeprofile"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofprobe"
)

const (
	ownFunc   = "github.com/acme/web/svc.Handle"
	otherFunc = "github.com/vendor/lib.Do"
)

// serviceProfile is what a Go runtime hands back from /debug/pprof/profile,
// built rather than captured so a test is fast and deterministic. It holds one
// frame of the workload's own module, one of a dependency, and one of the
// runtime, which is every class the filter has to tell apart.
func serviceProfile(t *testing.T) []byte {
	t.Helper()
	fn := func(id uint64, name, file string) *profile.Function {
		return &profile.Function{ID: id, Name: name, Filename: file, StartLine: 10}
	}
	own := fn(1, ownFunc, "/build/acme/web/svc/handle.go")
	other := fn(2, otherFunc, "/root/go/pkg/mod/vendor/lib.go")
	rt := fn(3, "runtime.mallocgc", "/usr/local/go/src/runtime/malloc.go")

	loc := func(id uint64, f *profile.Function) *profile.Location {
		return &profile.Location{ID: id, Address: 0x4000 + id, Line: []profile.Line{{Function: f, Line: 42}}}
	}
	ownLoc, otherLoc, rtLoc := loc(1, own), loc(2, other), loc(3, rt)

	mapping := &profile.Mapping{ID: 1, File: "/app/server", BuildID: "abcdef123456"}
	for _, l := range []*profile.Location{ownLoc, otherLoc, rtLoc} {
		l.Mapping = mapping
	}

	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		PeriodType: &profile.ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:     10_000_000,
		Mapping:    []*profile.Mapping{mapping},
		Function:   []*profile.Function{own, other, rt},
		Location:   []*profile.Location{ownLoc, otherLoc, rtLoc},
		Sample: []*profile.Sample{
			{Location: []*profile.Location{rtLoc, otherLoc, ownLoc}, Value: []int64{900},
				Label: map[string][]string{"customer_id": {"acme-42"}}},
			{Location: []*profile.Location{ownLoc}, Value: []int64{300}},
		},
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func candidate() pprofprobe.Candidate {
	return pprofprobe.Candidate{
		Target:       pprofprobe.Target{ImageDigest: "sha256:a", Port: 6060},
		Namespace:    "shop",
		WorkloadKind: "Deployment",
		WorkloadName: "web",
		Container:    "app",
		OwnModules:   []string{"github.com/acme/web"},
	}
}

type capture struct {
	mu      sync.Mutex
	pulled  []Pulled
	paths   []string
	queries []string
}

func (c *capture) sink(p Pulled) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pulled = append(c.pulled, p)
	return nil
}

func (c *capture) handler(t *testing.T, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.paths = append(c.paths, r.URL.Path)
		c.queries = append(c.queries, r.URL.RawQuery)
		c.mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, "Could not enable CPU profiling: cpu profiling already in use", status)
			return
		}
		_, _ = w.Write(serviceProfile(t))
	}
}

func newPuller(t *testing.T, srv *httptest.Server, sink Sink) *Puller {
	t.Helper()
	addr := strings.TrimPrefix(srv.URL, "http://")
	return New(nil, nodeprofile.ThirdPartyDrop,
		func(pprofprobe.Candidate) (string, bool) { return addr, true },
		sink, slog.New(slog.DiscardHandler))
}

// The whole path in one assertion: the workload's own frame survives because
// its build says which module it is, the dependency's does not, and nothing the
// runtime attached to the profile comes along.
func TestAPulledProfileKeepsOwnCodeAndNothingElse(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(c.handler(t, http.StatusOK))
	defer srv.Close()

	newPuller(t, srv, c.sink).cycle(t.Context(), []pprofprobe.Candidate{candidate()})

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pulled) != 1 {
		t.Fatalf("shipped %d profiles, want 1", len(c.pulled))
	}
	got := c.pulled[0]
	if got.WorkloadName != "web" || got.ImageDigest != "sha256:a" {
		t.Errorf("key = %+v", got)
	}
	p, err := profile.ParseData(got.Pprof)
	if err != nil {
		t.Fatalf("shipped bytes do not parse: %v", err)
	}
	names := map[string]bool{}
	for _, fn := range p.Function {
		names[fn.Name] = true
		if strings.Contains(fn.Filename, "/") {
			t.Errorf("filename %q is a path, not a base name", fn.Filename)
		}
	}
	if !names[ownFunc] {
		t.Error("the workload's own frame was redacted; the build states its module")
	}
	if names[otherFunc] {
		t.Error("a dependency frame survived under the drop policy")
	}
	if len(p.Mapping) != 0 {
		t.Errorf("mapping survived: it names the executable and its build ID")
	}
	for _, s := range p.Sample {
		if len(s.Label) != 0 {
			t.Errorf("sample label survived: %v", s.Label)
		}
	}
}

// Without the build's own modules the same profile has nothing left worth
// shipping — which is why the modules the binary states are not optional.
func TestWithoutTheBuildsModulesThereIsNothingToShip(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(c.handler(t, http.StatusOK))
	defer srv.Close()

	bare := candidate()
	bare.OwnModules = nil
	p := newPuller(t, srv, c.sink)
	p.cycle(t.Context(), []pprofprobe.Candidate{bare})

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pulled) != 0 {
		t.Errorf("shipped %d profiles of only runtime and placeholder frames", len(c.pulled))
	}
	if got := p.Snapshot(); got.Invalid != 1 {
		t.Errorf("coverage = %+v, want the profile counted as unshippable", got)
	}
}

// One path, one query. The mux beside it serves the process's argv.
func TestOnlyTheProfilePathIsRequested(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(c.handler(t, http.StatusOK))
	defer srv.Close()

	newPuller(t, srv, c.sink).cycle(t.Context(), []pprofprobe.Candidate{candidate()})

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.paths) != 1 || c.paths[0] != "/debug/pprof/profile" {
		t.Errorf("requested %v, want only the profile path", c.paths)
	}
	if c.queries[0] != "seconds=10" {
		t.Errorf("query = %q, want a ten-second capture", c.queries[0])
	}
}

// A process that will not start a profiler is one whose own profiler holds the
// single slot Go allows. It is left alone for hours, not retried.
func TestARefusalHoldsTheTargetForHours(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(c.handler(t, http.StatusInternalServerError))
	defer srv.Close()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	p := newPuller(t, srv, c.sink)
	p.now = func() time.Time { return now }

	p.cycle(t.Context(), []pprofprobe.Candidate{candidate()})
	if got := p.Snapshot(); got.Refused != 1 || got.Shipped != 0 {
		t.Fatalf("coverage = %+v, want one refusal and nothing shipped", got)
	}

	now = now.Add(refusedFor - time.Minute)
	p.cycle(t.Context(), []pprofprobe.Candidate{candidate()})
	c.mu.Lock()
	asks := len(c.paths)
	c.mu.Unlock()
	if asks != 1 {
		t.Errorf("asked %d times inside the hold window, want 1", asks)
	}

	now = now.Add(2 * time.Minute)
	p.cycle(t.Context(), []pprofprobe.Candidate{candidate()})
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.paths) != 2 {
		t.Errorf("asked %d times after the hold expired, want 2", len(c.paths))
	}
}

// A cycle is bounded, and it visits the least recently profiled first, so a
// large cluster is covered rather than the same few workloads repeatedly.
func TestACycleIsBoundedAndRoundRobin(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(c.handler(t, http.StatusOK))
	defer srv.Close()

	var candidates []pprofprobe.Candidate
	for i := range maxPullsPerCycle * 2 {
		cand := candidate()
		cand.Port = 6060 + i
		candidates = append(candidates, cand)
	}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	p := newPuller(t, srv, c.sink)
	p.now = func() time.Time { return now }

	p.cycle(t.Context(), candidates)
	c.mu.Lock()
	first := len(c.pulled)
	c.mu.Unlock()
	if first != maxPullsPerCycle {
		t.Fatalf("first cycle pulled %d, want the bound of %d", first, maxPullsPerCycle)
	}

	now = now.Add(time.Hour)
	p.cycle(t.Context(), candidates)
	c.mu.Lock()
	total := len(c.pulled)
	c.mu.Unlock()
	if total != 2*maxPullsPerCycle {
		t.Fatalf("second cycle pulled %d in total, want %d", total, 2*maxPullsPerCycle)
	}
	seen := map[int]bool{}
	for _, port := range portsOf(p) {
		seen[port] = true
	}
	if len(seen) != 2*maxPullsPerCycle {
		t.Errorf("visited %d distinct targets over two cycles, want %d — the second cycle repeated the first",
			len(seen), 2*maxPullsPerCycle)
	}
}

// portsOf lists the targets the puller has state for.
func portsOf(p *Puller) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int, 0, len(p.state))
	for t := range p.state {
		out = append(out, t.Port)
	}
	return out
}
