package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// capturedRequest records what the fake controller received.
type capturedRequest struct {
	mu     sync.Mutex
	auth   string
	report nodescan.Report
	hits   int
}

func fakeController(t *testing.T, captured *capturedRequest, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.mu.Lock()
		defer captured.mu.Unlock()
		captured.hits++
		captured.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&captured.report)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeToken(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sampleResult() nodescan.Result {
	return nodescan.Result{
		Binaries: []nodescan.BinaryInfo{{
			PID: 7, GoVersion: "go1.26.1", MainModule: "example.com/app",
			PGO: true, PodUID: "pod-uid", ContainerID: "container-id",
		}},
		Counters: nodescan.Counters{ProcessesScanned: 5, GoFound: 1, FilteredInfra: 2, Unreadable: 1},
	}
}

func TestNewReportShipperNilWithoutEndpoint(t *testing.T) {
	if s := newReportShipper("", "/tok", "node"); s != nil {
		t.Fatal("shipper should be nil (log-only) when no endpoint is configured")
	}
}

func TestReportShipperShipsWithBearerToken(t *testing.T) {
	captured := &capturedRequest{}
	srv := fakeController(t, captured, http.StatusAccepted)
	tokenPath := writeToken(t, "  secret-token\n") // padded to check trimming

	s := newReportShipper(srv.URL, tokenPath, "kind-worker")
	if err := s.ship(context.Background(), sampleResult(), nodescan.ProfilingCoverage{State: "supported"}); err != nil {
		t.Fatalf("ship: %v", err)
	}

	captured.mu.Lock()
	defer captured.mu.Unlock()
	if captured.auth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want %q", captured.auth, "Bearer secret-token")
	}
	if captured.report.Node != "kind-worker" || len(captured.report.Binaries) != 1 {
		t.Errorf("received report = %+v", captured.report)
	}
	if captured.report.Binaries[0].MainModule != "example.com/app" {
		t.Errorf("received binary = %+v", captured.report.Binaries[0])
	}
	if captured.report.Counters.FilteredInfra != 2 {
		t.Errorf("received counters = %+v", captured.report.Counters)
	}
}

func TestReportShipperReadsTokenPerSend(t *testing.T) {
	captured := &capturedRequest{}
	srv := fakeController(t, captured, http.StatusAccepted)
	tokenPath := writeToken(t, "first-token")

	s := newReportShipper(srv.URL, tokenPath, "n")
	if err := s.ship(context.Background(), sampleResult(), nodescan.ProfilingCoverage{State: "supported"}); err != nil {
		t.Fatalf("first ship: %v", err)
	}
	captured.mu.Lock()
	first := captured.auth
	captured.mu.Unlock()

	// Rotate the token file; the next send must pick up the new value.
	if err := os.WriteFile(tokenPath, []byte("rotated-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.ship(context.Background(), sampleResult(), nodescan.ProfilingCoverage{State: "supported"}); err != nil {
		t.Fatalf("second ship: %v", err)
	}
	captured.mu.Lock()
	second := captured.auth
	captured.mu.Unlock()

	if first != "Bearer first-token" {
		t.Errorf("first auth = %q", first)
	}
	if second != "Bearer rotated-token" {
		t.Errorf("second auth = %q, want the rotated token", second)
	}
}

func TestReportShipperErrorsOnNon2xx(t *testing.T) {
	captured := &capturedRequest{}
	srv := fakeController(t, captured, http.StatusUnauthorized)
	tokenPath := writeToken(t, "tok")

	s := newReportShipper(srv.URL, tokenPath, "n")
	if err := s.ship(context.Background(), sampleResult(), nodescan.ProfilingCoverage{State: "supported"}); err == nil {
		t.Fatal("ship should error on a non-2xx response")
	}
}

func TestReportShipperErrorsOnMissingToken(t *testing.T) {
	captured := &capturedRequest{}
	srv := fakeController(t, captured, http.StatusAccepted)

	s := newReportShipper(srv.URL, filepath.Join(t.TempDir(), "absent"), "n")
	if err := s.ship(context.Background(), sampleResult(), nodescan.ProfilingCoverage{State: "supported"}); err == nil {
		t.Fatal("ship should error when the token file is absent")
	}
	captured.mu.Lock()
	defer captured.mu.Unlock()
	if captured.hits != 0 {
		t.Error("no request should be sent without a token")
	}
}
