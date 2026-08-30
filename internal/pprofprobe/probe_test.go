package pprofprobe

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/pprof"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/inventory"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

func testProber(t *testing.T, addr Address) *Prober {
	t.Helper()
	return New(addr, slog.New(slog.DiscardHandler))
}

// fixedAddress answers every candidate with one address.
func fixedAddress(addr string) Address {
	return func(Candidate) (string, bool) { return addr, true }
}

func candidate(digest string, port int) Candidate {
	return Candidate{
		Target:       Target{ImageDigest: digest, Port: port},
		Namespace:    "shop",
		WorkloadKind: "Deployment",
		WorkloadName: "web",
		Container:    "app",
	}
}

// The real handler from the standard library, so what is matched is the page Go
// actually serves rather than a fixture written from memory.
func TestARealPprofIndexIsConfirmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(pprof.Index))
	defer srv.Close()

	p := testProber(t, fixedAddress(hostOf(t, srv.URL)))
	p.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})

	if got := p.Snapshot(); got.Confirmed != 1 {
		t.Errorf("coverage = %+v, want one confirmed", got)
	}
	want := []Target{{ImageDigest: "sha256:a", Port: 6060}}
	if got := p.Endpoints(); !reflect.DeepEqual(got, want) {
		t.Errorf("endpoints = %+v, want %+v", got, want)
	}
}

// A 200 from something that is not the pprof mux must not confirm: the status
// says only that a server answered on the port.
func TestSomethingElseOnThePortIsAbsentNotConfirmed(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"a healthy application", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<html><title>my service</title></html>")
		}},
		{"a router that does not know the path", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}},
		{"an empty 200", func(http.ResponseWriter, *http.Request) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			p := testProber(t, fixedAddress(hostOf(t, srv.URL)))
			p.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})

			if got := p.Snapshot(); got.Absent != 1 {
				t.Errorf("coverage = %+v, want one absent", got)
			}
		})
	}
}

// The one path this package may fetch. `/debug/pprof/cmdline` returns the
// process's own argv, which is the field the agent drops at the source — no
// request may ever ask for it.
func TestOnlyTheIndexPathIsRequested(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		pprof.Index(w, r)
	}))
	defer srv.Close()

	p := testProber(t, fixedAddress(hostOf(t, srv.URL)))
	p.round(t.Context(), []Candidate{candidate("sha256:a", 6060), candidate("sha256:b", 8080)})

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"/debug/pprof/", "/debug/pprof/"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("requested %v, want only the index path", paths)
	}
}

// A redirect could send the probe to an address the funnel never approved.
func TestARedirectIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/debug/pprof/", http.StatusFound)
	}))
	defer srv.Close()

	p := testProber(t, fixedAddress(hostOf(t, srv.URL)))
	p.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})

	if got := p.Snapshot(); got.Unreachable != 1 {
		t.Errorf("coverage = %+v, want the redirect refused rather than followed", got)
	}
}

// The two answers about the endpoint are terminal; the one about the network is
// not. A build is asked about once, however many rounds run.
func TestAConfirmedTargetIsNeverAskedAgain(t *testing.T) {
	var mu sync.Mutex
	asks := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asks++
		mu.Unlock()
		pprof.Index(w, r)
	}))
	defer srv.Close()

	p := testProber(t, fixedAddress(hostOf(t, srv.URL)))
	for range 5 {
		p.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})
	}
	mu.Lock()
	defer mu.Unlock()
	if asks != 1 {
		t.Errorf("asked %d times over five rounds, want 1 — the answer is about the build", asks)
	}
}

// An unreachable target says nothing about the endpoint, so it expires.
func TestAnUnreachableTargetIsAskedAgainLater(t *testing.T) {
	// A closed port: nothing listens, so the dial fails at once.
	srv := httptest.NewServer(http.HandlerFunc(pprof.Index))
	addr := hostOf(t, srv.URL)
	srv.Close()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	p := testProber(t, fixedAddress(addr))
	p.now = func() time.Time { return now }

	p.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})
	if got := p.Snapshot(); got.Unreachable != 1 {
		t.Fatalf("coverage = %+v, want one unreachable", got)
	}
	if p.due(Target{ImageDigest: "sha256:a", Port: 6060}) {
		t.Error("an unreachable target is due again immediately; the retry window is not held")
	}
	now = now.Add(unreachableRetry + time.Second)
	if !p.due(Target{ImageDigest: "sha256:a", Port: 6060}) {
		t.Error("an unreachable target is never due again; the retry window does not expire")
	}
}

// A cluster that grows by a thousand images must not answer for all of them in
// one round.
func TestARoundIsBounded(t *testing.T) {
	var mu sync.Mutex
	asks := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asks++
		mu.Unlock()
		pprof.Index(w, r)
	}))
	defer srv.Close()

	var candidates []Candidate
	for i := range maxProbesPerTick * 3 {
		candidates = append(candidates, candidate("sha256:"+string(rune('a'+i%26))+string(rune('a'+i/26)), 6060))
	}
	p := testProber(t, fixedAddress(hostOf(t, srv.URL)))
	p.round(t.Context(), candidates)

	mu.Lock()
	defer mu.Unlock()
	if asks != maxProbesPerTick {
		t.Errorf("asked %d times in one round, want the bound of %d", asks, maxProbesPerTick)
	}
}

// A workload with no addressable pod right now is skipped, not answered: an
// answer would be cached and the workload never asked about again.
func TestNoAddressLeavesTheTargetUnanswered(t *testing.T) {
	p := testProber(t, func(Candidate) (string, bool) { return "", false })
	p.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})

	if got := (p.Snapshot()); got != (Coverage{}) {
		t.Errorf("coverage = %+v, want nothing recorded", got)
	}
	if !p.due(Target{ImageDigest: "sha256:a", Port: 6060}) {
		t.Error("the target is not due again; a missing address became an answer")
	}
}

func TestCandidatesAreTheFunnelsOutput(t *testing.T) {
	ports := []inventory.PortRecord{
		{
			PortKey: inventory.PortKey{
				Key:         inventory.Key{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"},
				ImageDigest: "sha256:linked",
			},
			Ports: []nodescan.ListeningPort{{Port: 8080}, {Port: 6060, Loopback: true}},
		},
		{
			PortKey: inventory.PortKey{
				Key:         inventory.Key{Namespace: "shop", WorkloadKind: "StatefulSet", WorkloadName: "db", Container: "db"},
				ImageDigest: "sha256:plain",
			},
			Ports: []nodescan.ListeningPort{{Port: 5432}},
		},
	}
	got := Candidates(ports, map[string][]string{"sha256:linked": {"github.com/acme/web"}})

	want := []Candidate{{
		Target:       Target{ImageDigest: "sha256:linked", Port: 8080},
		Namespace:    "shop",
		WorkloadKind: "Deployment",
		WorkloadName: "web",
		Container:    "app",
		OwnModules:   []string{"github.com/acme/web"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("candidates = %+v, want %+v (loopback and unlinked builds are not asked about)", got, want)
	}
}

// hostOf strips the scheme from an httptest URL, leaving the host:port the
// Address contract returns.
func hostOf(t *testing.T, url string) string {
	t.Helper()
	const prefix = "http://"
	if len(url) <= len(prefix) || url[:len(prefix)] != prefix {
		t.Fatalf("unexpected test server URL %q", url)
	}
	return url[len(prefix):]
}
