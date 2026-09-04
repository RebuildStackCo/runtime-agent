package health

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// serve returns a live server over the two checks, and the base URL to ask.
func serve(t *testing.T, live, ready Check) string {
	t.Helper()
	s := New("127.0.0.1:0", live, ready, testLogger())
	// httptest owns the listener so the test never picks a port; what is under
	// test is the handler wiring, which New built.
	srv := httptest.NewServer(s.handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func get(t *testing.T, method, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestTheTwoQuestionsAreAnsweredSeparately(t *testing.T) {
	// The state this endpoint exists to distinguish: a process that is working
	// and has not finished filling its caches. Liveness must say yes there, or
	// the kubelet restarts the agent in the middle of its first sync.
	base := serve(t,
		func() (bool, string) { return true, "" },
		func() (bool, string) { return false, "informer caches syncing" })

	if code, body := get(t, http.MethodGet, base+LivePath); code != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Errorf("GET %s = %d %q, want 200 ok", LivePath, code, body)
	}
	code, body := get(t, http.MethodGet, base+ReadyPath)
	if code != http.StatusServiceUnavailable {
		t.Errorf("GET %s = %d, want 503 while the caches fill", ReadyPath, code)
	}
	if strings.TrimSpace(body) != "informer caches syncing" {
		t.Errorf("body = %q, want the check's reason", body)
	}
}

// CLAUDE.md invariant 1: nothing arriving at the agent changes what it does.
// The endpoint has no verb but read, and a request that tries one is refused
// rather than quietly served.
func TestNothingAboutARequestReachesTheAgent(t *testing.T) {
	var calls int
	base := serve(t,
		func() (bool, string) { calls++; return true, "" },
		func() (bool, string) { return true, "" })

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if code, _ := get(t, method, base+LivePath); code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, LivePath, code)
		}
	}
	if calls != 0 {
		t.Errorf("the check ran %d times for methods that are not reads", calls)
	}
	// A query string is the other way a caller could try to say something. It
	// is not parsed: the path is the whole of the request.
	if code, _ := get(t, http.MethodGet, base+LivePath+"?ready=false&spool=/etc"); code != http.StatusOK {
		t.Errorf("a query string changed the answer: %d", code)
	}
	if code, _ := get(t, http.MethodHead, base+LivePath); code != http.StatusOK {
		t.Errorf("HEAD %s = %d, want 200: kubelet probes may use it", LivePath, code)
	}
}

func TestAnUnknownPathIsNotAnAnswer(t *testing.T) {
	ok := func() (bool, string) { return true, "" }
	base := serve(t, ok, ok)
	if code, _ := get(t, http.MethodGet, base+"/metrics"); code != http.StatusNotFound {
		t.Errorf("GET /metrics = %d, want 404 until something serves it", code)
	}
}

func TestTheListenerStopsWithItsContext(t *testing.T) {
	ok := func() (bool, string) { return true, "" }
	s := New("127.0.0.1:0", ok, ok, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on a canceled context, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was canceled")
	}
}
