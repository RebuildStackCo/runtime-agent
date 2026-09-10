package shipper

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

// The rule this package exists for: a spool file is deleted only after a 2xx.
// Every other answer leaves the payload where it is, which is the difference
// between an outage and data loss nobody detects (ADR 0075).
func TestOnlyA2xxRemovesAPayload(t *testing.T) {
	cases := []struct {
		status  int
		removed bool
		want    model.Shipping
	}{
		{status: 200, removed: true, want: model.Shipping{Delivered: 1}},
		{status: 202, removed: true, want: model.Shipping{Delivered: 1}},
		{status: 204, removed: true, want: model.Shipping{Delivered: 1}},
		{status: 400, want: model.Shipping{Rejected: model.ShippingRejections{Malformed: 1}}},
		{status: 401, want: model.Shipping{Halted: true, Rejected: model.ShippingRejections{Unauthorized: 1}}},
		{status: 403, want: model.Shipping{Halted: true, Rejected: model.ShippingRejections{Unauthorized: 1}}},
		{status: 413, want: model.Shipping{Rejected: model.ShippingRejections{TooLarge: 1}}},
		{status: 429, want: model.Shipping{Deferred: 1}},
		{status: 500, want: model.Shipping{Deferred: 1}},
		{status: 503, want: model.Shipping{Deferred: 1}},
		// Not one of the classes above, and deliberately transient: a base URL
		// with a typo answers this to every payload, and treating it as
		// permanent would empty the spool against a configuration mistake.
		{status: 404, want: model.Shipping{Deferred: 1}},
		{status: 418, want: model.Shipping{Deferred: 1}},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.status), func(t *testing.T) {
			backend := answering(c.status)
			s, dir := newShipper(t, backend.URL)
			payload(t, dir, "collection-coverage.json", "collection_coverage")

			s.Round(t.Context())

			if _, err := os.Stat(filepath.Join(dir, "collection-coverage.json")); os.IsNotExist(err) != c.removed {
				t.Errorf("status %d: file removed=%v, want removed=%v", c.status, !c.removed, c.removed)
			}
			got := s.Coverage()
			// Both sides of the encoding are counted for every attempt that
			// reached the wire, whatever the answer was (ADR 0076).
			if got.PayloadBytes == 0 || got.TransmittedBytes == 0 {
				t.Errorf("status %d: an attempt that reached the backend counted no bytes: %+v", c.status, got)
			}
			got.PayloadBytes, got.TransmittedBytes = 0, 0
			if got != c.want {
				t.Errorf("status %d: coverage %+v, want %+v", c.status, got, c.want)
			}
		})
	}
}

// The endpoint is one payload per request at the kind's registry name, and the
// body is the spool file under a declared, reversible encoding. There is no
// envelope and no batch.
//
// Decoding here rather than comparing raw bytes is the sink invariant stated on
// the wire: the payload the local sink writes is the payload that travels, and
// gzip is how it travels rather than part of what it is (ADR 0076).
func TestOnePayloadPerRequestAtTheKindsRegistryName(t *testing.T) {
	var gotPath, gotType, gotEncoding string
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotPath, gotType = r.URL.Path, r.Header.Get("Content-Type")
		gotEncoding = r.Header.Get("Content-Encoding")
		if r.Method != http.MethodPost {
			t.Errorf("method %s, want POST", r.Method)
		}
	}))
	defer backend.Close()

	s, dir := newShipper(t, backend.URL+"/ingest/")
	want := payload(t, dir, "usage-1-3600.json", "usage_snapshot")

	s.Round(t.Context())

	if gotPath != "/ingest/v1/kubernetes/usage_snapshot" {
		t.Errorf("path %q", gotPath)
	}
	if gotType != "application/json" {
		t.Errorf("content type %q", gotType)
	}
	if gotEncoding != "gzip" {
		t.Errorf("content encoding %q, want gzip", gotEncoding)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gotBody))
	if err != nil {
		t.Fatalf("the body did not decode as the encoding it declared: %v", err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading the decoded body: %v", err)
	}
	if string(decoded) != want {
		t.Errorf("the decoded body was not the spool file verbatim:\ngot  %s\nwant %s", decoded, want)
	}
}

// A payload the backend will never accept is held, not retried and not deleted:
// the bytes that prove the disagreement stay readable until the spool's own age
// bound removes them (ADR 0075).
func TestAPermanentlyRejectedPayloadIsNeverOfferedAgain(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int64
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(status)
			}))
			defer backend.Close()

			s, dir := newShipper(t, backend.URL)
			payload(t, dir, "collection-coverage.json", "collection_coverage")

			for range 3 {
				s.Round(t.Context())
			}

			if got := requests.Load(); got != 1 {
				t.Errorf("offered %d times, want 1: a rejected payload must not be retried", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "collection-coverage.json")); err != nil {
				t.Errorf("the rejected payload was removed: %v", err)
			}
		})
	}
}

// The mark is on the bytes, not on the name. A superseding kind writes a new
// payload under the same filename every pass, and refusing to offer that one
// because its predecessor was rejected would stop the kind for good.
func TestAReplacedPayloadIsOfferedAgainAfterARejection(t *testing.T) {
	var requests atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer backend.Close()

	s, dir := newShipper(t, backend.URL)
	payload(t, dir, "workload-metadata.json", "workload_metadata")
	s.Round(t.Context())

	s.Round(t.Context()) // the same file: held, not offered
	if got := requests.Load(); got != 1 {
		t.Fatalf("the held payload was offered again: %d requests", got)
	}

	payload(t, dir, "workload-metadata.json", "workload_metadata") // a new pass replaces it
	s.Round(t.Context())

	if got := requests.Load(); got != 2 {
		t.Errorf("%d requests, want 2: replacing the file must clear the mark", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "workload-metadata.json")); err == nil {
		t.Error("the replacement was delivered and should have been removed")
	}
}

// A retry storm against an authentication failure is how a fleet takes down the
// thing it is trying to reach. There is nothing to retry into, so shipping
// stops — and collection does not.
func TestAnIdentityFailureStopsShipping(t *testing.T) {
	var requests atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer backend.Close()

	s, dir := newShipper(t, backend.URL)
	payload(t, dir, "collection-coverage.json", "collection_coverage")
	payload(t, dir, "oom-1.json", "oom_kill")

	for range 3 {
		s.Round(t.Context())
	}

	if got := requests.Load(); got != 1 {
		t.Errorf("%d requests after a 403, want 1", got)
	}
	if !s.Coverage().Halted {
		t.Error("coverage does not say shipping halted")
	}
	if names := spoolNames(t, dir); len(names) != 2 {
		t.Errorf("the spool holds %v, want both payloads", names)
	}
}

// Run must not return on a halt. In cmd/agent a lifecycle task that returns
// cancels the whole agent, so a backend refusing this agent's identity would
// stop collection — the one thing shipping may never do.
func TestRunOutlivesAHaltAndStopsOnlyOnCancellation(t *testing.T) {
	backend := answering(http.StatusUnauthorized)
	s, dir := newShipper(t, backend.URL)
	s.interval = time.Millisecond
	payload(t, dir, "collection-coverage.json", "collection_coverage")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitFor(t, "shipping to halt", func() bool { return s.Coverage().Halted })
	select {
	case <-done:
		t.Fatal("Run returned on a halt; that would stop collection too")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on cancellation")
	}
}

// An unreachable backend changes nothing about what the agent collects or what
// its spool holds. This is the failure the whole design is arranged around.
func TestAnUnreachableBackendLeavesTheSpoolUntouched(t *testing.T) {
	// A server that is closed before the first request: the address is real and
	// nothing is listening on it, which is a rescheduled backend exactly.
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := backend.URL
	backend.Close()

	s, dir := newShipper(t, address)
	before := map[string]string{
		"collection-coverage.json": payload(t, dir, "collection-coverage.json", "collection_coverage"),
		"oom-1.json":               payload(t, dir, "oom-1.json", "oom_kill"),
	}

	for range 3 {
		s.Round(t.Context())
	}

	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- test-controlled path
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s changed under an unreachable backend", name)
		}
	}
	if got := s.Coverage(); got.Deferred != 3 || got.Delivered != 0 || got.Halted {
		t.Errorf("coverage %+v: three deferred attempts, nothing delivered, no halt", got)
	}
}

// One request at a time, and never two for one natural key. That is what lets a
// superseding payload carry no ordering field: a newer snapshot cannot overtake
// an older one if the older one's request has already finished (ADR 0027 §2).
func TestOneRequestIsInFlightAtATimeAndOldestGoesFirst(t *testing.T) {
	var mu sync.Mutex
	var live, peak int
	var order []string
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		order = append(order, r.URL.Path)
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		live--
		mu.Unlock()
	}))
	defer backend.Close()

	s, dir := newShipper(t, backend.URL)
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"c.json", "b.json", "a.json"} {
		payload(t, dir, name, "oom_kill")
		aged(t, dir, name, base.Add(time.Duration(i)*time.Minute))
	}
	// A fourth, older than all of them and of another kind, so the order is
	// visible in the paths.
	payload(t, dir, "d.json", "collection_coverage")
	aged(t, dir, "d.json", base.Add(-time.Minute))

	s.Round(t.Context())

	mu.Lock()
	defer mu.Unlock()
	if peak != 1 {
		t.Errorf("%d requests were in flight at once, want 1", peak)
	}
	want := []string{
		"/v1/kubernetes/collection_coverage",
		"/v1/kubernetes/oom_kill",
		"/v1/kubernetes/oom_kill",
		"/v1/kubernetes/oom_kill",
	}
	if len(order) != len(want) || order[0] != want[0] {
		t.Errorf("request order %v, want the oldest payload first: %v", order, want)
	}
	if names := spoolNames(t, dir); len(names) != 0 {
		t.Errorf("the spool still holds %v", names)
	}
}

// A superseding writer replaces a file by rename while its predecessor is in
// flight. Deleting by name after the 2xx would drop a newer payload that was
// never offered, and nothing downstream could notice.
func TestAPayloadReplacedMidFlightSurvivesItsPredecessorsAcknowledgment(t *testing.T) {
	var dir string
	var replaced string
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		if replaced == "" {
			replaced = payload(t, dir, "workload-metadata.json", "workload_metadata")
		}
	}))
	defer backend.Close()

	s, d := newShipper(t, backend.URL)
	dir = d
	payload(t, dir, "workload-metadata.json", "workload_metadata")

	s.Round(t.Context())

	got, err := os.ReadFile(filepath.Join(dir, "workload-metadata.json")) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatalf("the newer payload was deleted on its predecessor's acknowledgment: %v", err)
	}
	if string(got) != replaced {
		t.Error("the file in the spool is not the newer payload")
	}
	if c := s.Coverage(); c.Delivered != 1 {
		t.Errorf("coverage %+v, want one delivery", c)
	}
}

// Past the ceiling is a 413 the agent can answer for itself. It costs the
// backend nothing and reaches the same outcome (ADR 0075).
func TestAPayloadPastTheCeilingIsNeverOffered(t *testing.T) {
	var requests atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
	}))
	defer backend.Close()

	s, dir := newShipper(t, backend.URL)
	name := "usage-1-3600.json"
	body := fmt.Sprintf(`{"kind":"usage_snapshot","source":"measured","pad":%q}`+"\n",
		strings.Repeat("x", MaxPayloadBytes))
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	s.Round(t.Context())
	s.Round(t.Context())

	if got := requests.Load(); got != 0 {
		t.Errorf("%d requests, want none: the payload is past the agent's own ceiling", got)
	}
	if c := s.Coverage(); c.Rejected.TooLarge != 1 {
		t.Errorf("coverage %+v, want one too_large", c)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Errorf("the oversized payload was removed: %v", err)
	}
}

// The path segment is a registry name or the payload does not go. Anything else
// in the directory is left alone and counted, never deleted and never turned
// into a URL (ADR 0022).
func TestAFileThatIsNotAPayloadThisAgentShipsIsLeftAlone(t *testing.T) {
	var requests atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
	}))
	defer backend.Close()

	s, dir := newShipper(t, backend.URL)
	files := map[string]string{
		"truncated.json":    `{"kind":"usage_sna`,
		"nokind.json":       `{"source":"measured"}`,
		"invented.json":     `{"kind":"../../etc/passwd","source":"measured"}`,
		"unregistered.json": `{"kind":"cluster_secrets","source":"structural"}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A `.tmp` file is a write in progress and not a payload at all.
	if err := os.WriteFile(filepath.Join(dir, "usage-1-3600.json.tmp"), []byte(`{"kind":"usage_snapshot"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s.Round(t.Context())

	if got := requests.Load(); got != 0 {
		t.Errorf("%d requests, want none", got)
	}
	if c := s.Coverage(); c.Unreadable != int64Len(files) {
		t.Errorf("coverage %+v, want %d unreadable", c, len(files))
	}
	if names := spoolNames(t, dir); len(names) != len(files)+1 {
		t.Errorf("the spool holds %v; nothing should have been deleted", names)
	}
}

// The round stops at the first payload the backend could not take: the next one
// would meet the same backend, and a full spool against a down backend is how a
// fleet turns an outage into an outage that will not end.
func TestARoundStopsAtTheFirstDeferral(t *testing.T) {
	var requests atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer backend.Close()

	s, dir := newShipper(t, backend.URL)
	for _, name := range []string{"a.json", "b.json", "c.json"} {
		payload(t, dir, name, "oom_kill")
	}

	s.Round(t.Context())

	if got := requests.Load(); got != 1 {
		t.Errorf("%d requests in one round, want 1", got)
	}
	if names := spoolNames(t, dir); len(names) != 3 {
		t.Errorf("the spool holds %v, want all three", names)
	}
}

// Nothing is read out of a response but its status. ADR 0001 forbids the
// backend to send anything the agent acts on; here that stops being a promise
// about the backend and becomes a property of the agent.
func TestNothingIsReadOutOfAResponseButItsStatus(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Ingest-Interval-Seconds", "1")
		w.Header().Set("Retry-After", "3600")
		_, _ = io.WriteString(w, `{"interval_seconds":1,"max_payload_bytes":1,"halt":true,"drop":["oom_kill"]}`)
	}))
	defer backend.Close()

	s, dir := newShipper(t, backend.URL)
	payload(t, dir, "oom-1.json", "oom_kill")

	s.Round(t.Context())

	if c := s.Coverage(); c.Delivered != 1 || c.Halted {
		t.Errorf("coverage %+v: the body was acted on", c)
	}
	if s.interval != DefaultInterval {
		t.Errorf("the round interval moved to %v; the response body is not configuration", s.interval)
	}
	if s.nextDelay() != DefaultInterval {
		t.Errorf("the next round is %v away; Retry-After is a header the agent does not read", s.nextDelay())
	}
}

// After an outage every agent in a fleet has a full spool and the same idea at
// the same moment. The backend is required to tolerate a catch-up burst; the
// agent is required not to make it a stampede.
func TestTheBackoffGrowsAndIsSpread(t *testing.T) {
	s, _ := newShipper(t, "http://backend.invalid")
	s.jitter = func(int64) int64 { return 0 } // the bottom of each window

	if got := s.nextDelay(); got != DefaultInterval {
		t.Errorf("with nothing failing the delay is %v, want %v", got, DefaultInterval)
	}

	var last time.Duration
	for i := 1; i <= 12; i++ {
		s.failures = i
		got := s.nextDelay()
		if got < minBackoff/2 || got > maxBackoff {
			t.Fatalf("after %d failures the delay is %v, outside [%v, %v]", i, got, minBackoff/2, maxBackoff)
		}
		if i > 1 && got < last {
			t.Errorf("after %d failures the delay fell from %v to %v", i, last, got)
		}
		last = got
	}
	if last != maxBackoff/2 {
		t.Errorf("the backoff settled at %v, want the bottom of the %v ceiling's window", last, maxBackoff)
	}

	// Two agents that failed together do not return together.
	s.failures = 6
	s.jitter = func(int64) int64 { return 0 }
	low := s.nextDelay()
	s.jitter = func(n int64) int64 { return n - 1 }
	high := s.nextDelay()
	if low >= high || low*2 <= high {
		t.Errorf("jitter window [%v, %v]: half the delay should be fixed and half spread", low, high)
	}
}

// A delivery resets the backoff: the next round is one interval away, not the
// five minutes the outage had climbed to.
func TestADeliveryResetsTheBackoff(t *testing.T) {
	backend := answering(http.StatusOK)
	s, dir := newShipper(t, backend.URL)
	s.failures = 8
	payload(t, dir, "oom-1.json", "oom_kill")

	s.Round(t.Context())

	if got := s.nextDelay(); got != DefaultInterval {
		t.Errorf("after a delivery the next round is %v away, want %v", got, DefaultInterval)
	}
}

// No default, and an absent backend is the behaviour of every installation that
// existed before there was a shipper.
func TestAnAgentWithNoBackendHasNoShipper(t *testing.T) {
	if s := New("", t.TempDir(), discardLogger()); s != nil {
		t.Errorf("New returned %v for an empty base URL", s)
	}
}

// The bytes the sink writes are the bytes that go on the wire, and the kind in
// the URL is the kind in those bytes. Real payloads rather than fixtures,
// because the registry check is what this is about (ADR 0022, ADR 0012).
func TestTheKindsTheSinkWritesAreTheKindsTheEndpointNames(t *testing.T) {
	var paths []string
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
	}))
	defer backend.Close()

	dir := t.TempDir()
	spool, err := sink.NewSpool(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.WriteOOMKill(model.OOMKill{Namespace: "shop", Pod: "web-1", Container: "web",
		FinishedAt: time.Unix(1, 0).UTC(), RestartCount: 2}); err != nil {
		t.Fatal(err)
	}
	if err := spool.WriteCollectionCoverage(time.Unix(2, 0).UTC(), time.Unix(1, 0).UTC(),
		sink.AgentInfo{Version: "test"}, nil, model.Coverage{}, model.PlacementDrops{},
		model.NodeDrops{}, nil, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	s := New(backend.URL, dir, discardLogger())
	s.Round(t.Context())

	want := map[string]bool{
		"/v1/kubernetes/oom_kill":            true,
		"/v1/kubernetes/collection_coverage": true,
	}
	if len(paths) != len(want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
	for _, p := range paths {
		if !want[p] {
			t.Errorf("unexpected path %q", p)
		}
	}
	if names := spoolNames(t, dir); len(names) != 0 {
		t.Errorf("the spool still holds %v", names)
	}
}

// A process killed mid-request loses nothing: the file is the truth until a
// 2xx, and the next process finds it exactly where it was.
func TestAnInterruptedDeliveryLeavesThePayloadToBeSentAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	// The request stays open until the test lets go, so the delivery is
	// interrupted by the cancellation and by nothing else.
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		cancel() // the agent is stopping while this request is open
		<-release
	}))
	defer backend.Close()
	defer close(release)

	s, dir := newShipper(t, backend.URL)
	want := payload(t, dir, "oom-1.json", "oom_kill")

	s.Round(ctx)

	got, err := os.ReadFile(filepath.Join(dir, "oom-1.json")) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatalf("an interrupted delivery removed the payload: %v", err)
	}
	if string(got) != want {
		t.Error("the payload changed")
	}
	if c := s.Coverage(); c.Delivered != 0 {
		t.Errorf("coverage %+v, want nothing delivered", c)
	}
}

// --- helpers ---

// The property the whole final round is bounded for: a backend that never
// answers must not be able to hold up a pod's termination. Everything else
// about this round is best effort; this is not (ADR 0075, ADR 0079).
func TestAHangingBackendCannotHoldUpShutdown(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer backend.Close()
	defer close(release)

	s, dir := newShipper(t, backend.URL)
	s.finalBudget = 200 * time.Millisecond
	payload(t, dir, "collection-coverage.json", "collection_coverage")

	start := time.Now()
	s.FinalRound(t.Context())
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("the final round took %s against a %s budget; a hung backend delays shutdown",
			elapsed, s.finalBudget)
	}
	// Undelivered is undelivered: the file stays, exactly as in any other round.
	if _, err := os.Stat(filepath.Join(dir, "collection-coverage.json")); err != nil {
		t.Errorf("an undelivered payload was removed from the spool: %v", err)
	}
}

// The round is the ordinary one under a deadline, so what it delivers it
// deletes and what it does not it leaves.
func TestTheFinalRoundDeliversWhatItCanAndKeepsTheRest(t *testing.T) {
	backend := answering(http.StatusOK)
	defer backend.Close()

	s, dir := newShipper(t, backend.URL)
	payload(t, dir, "collection-coverage.json", "collection_coverage")
	payload(t, dir, "usage-1-3600.json", "usage_snapshot")

	s.FinalRound(t.Context())

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the spool still holds %d payloads a 200 acknowledged", len(entries))
	}
	if got := s.Coverage().Delivered; got != 2 {
		t.Errorf("delivered %d, want 2", got)
	}
}

// Nothing retries into a rejected credential, and the last round least of all:
// it would be a fleet's worth of spools arriving at once at a backend that has
// already said no.
func TestAHaltedShipperRunsNoFinalRound(t *testing.T) {
	var requests atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer backend.Close()

	s, dir := newShipper(t, backend.URL)
	payload(t, dir, "collection-coverage.json", "collection_coverage")

	s.Round(t.Context()) // the 401 that halts it
	if !s.halted.Load() {
		t.Fatal("a 401 did not halt the shipper")
	}
	before := requests.Load()

	s.FinalRound(t.Context())

	if got := requests.Load(); got != before {
		t.Errorf("the final round made %d more requests into a rejected credential", got-before)
	}
}

func newShipper(t *testing.T, base string) (*Shipper, string) {
	t.Helper()
	dir := t.TempDir()
	s := New(base, dir, discardLogger())
	if s == nil {
		t.Fatalf("New returned nil for base %q", base)
	}
	return s, dir
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func answering(status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
}

// payload writes one plausible payload the way the sink does — a temp file
// renamed into place — so a rewrite produces a new file rather than new bytes
// in the old one. It returns what it wrote.
func payload(t *testing.T, dir, name, kind string) string {
	t.Helper()
	body := fmt.Sprintf("{\n  \"kind\": %q,\n  \"source\": \"agent\",\n  \"written\": %d\n}\n",
		kind, time.Now().UnixNano())
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	return body
}

func aged(t *testing.T, dir, name string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(filepath.Join(dir, name), at, at); err != nil {
		t.Fatal(err)
	}
}

func spoolNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func int64Len[T any](m map[string]T) uint64 { return uint64(len(m)) }
