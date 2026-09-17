package decisions

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

func testHandler(list []collector.Decision, synced bool) http.Handler {
	return Handler(
		func() []collector.Decision { return list },
		func() bool { return synced },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

var sample = []collector.Decision{
	{Kind: "Namespace", Name: "shop", Collected: true, Profiled: true},
	{Kind: "Pod", Namespace: "shop", Name: "web-a", Collected: true, Profiled: true},
	{Kind: "Pod", Namespace: "shop", Name: "web-b", Collected: false,
		Reason: collector.ExcludedByPodAnnotation},
}

func TestTheListIsOneObjectPerLine(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(sample, true).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Errorf("content type = %q, want ndjson", ct)
	}

	var got []collector.Decision
	scan := bufio.NewScanner(rec.Body)
	for scan.Scan() {
		var d collector.Decision
		if err := json.Unmarshal(scan.Bytes(), &d); err != nil {
			t.Fatalf("line %q is not an object: %v", scan.Text(), err)
		}
		got = append(got, d)
	}
	if len(got) != len(sample) {
		t.Fatalf("read %d lines, want %d", len(got), len(sample))
	}
	if got[2].Reason != collector.ExcludedByPodAnnotation {
		t.Errorf("reason = %q, want the pod annotation", got[2].Reason)
	}
}

// The reason is the point of the endpoint, so an absent one has to be absent
// rather than an empty string a reader has to interpret.
func TestACollectedObjectCarriesNoReason(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(sample[:1], true).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil))

	if strings.Contains(rec.Body.String(), "reason") {
		t.Errorf("a collected object was written with a reason field: %s", rec.Body.String())
	}
}

// Nothing about a request changes what this endpoint does, and a method that
// is not a read is refused rather than treated as one (invariant 1).
func TestOnlyReadsAreAnswered(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		testHandler(sample, true).ServeHTTP(rec, httptest.NewRequest(method, Path, strings.NewReader("{}")))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	testHandler(sample, true).ServeHTTP(rec, httptest.NewRequest(http.MethodHead, Path, nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD = %d with %d bytes, want 200 and no body", rec.Code, rec.Body.Len())
	}
}

// A query is not a filter here. Everything is answered whatever the URL says,
// so a reader cannot come to believe an empty answer means nothing matched.
func TestAQueryChangesNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(sample, true).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, Path+"?only=excluded&namespace=nowhere", nil))

	if lines := strings.Count(strings.TrimSpace(rec.Body.String()), "\n") + 1; lines != len(sample) {
		t.Errorf("read %d lines with a query, want all %d", lines, len(sample))
	}
}

// A partial list is the failure this endpoint exists to prevent: it would look
// exactly like a cluster with fewer objects in it.
func TestAnUnsyncedCacheAnswersNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(sample, false).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 while the caches are filling", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "shop") {
		t.Errorf("an unsynced answer named an object: %s", rec.Body.String())
	}
}

// Only Path is served. A listener that answered elsewhere would be a second
// surface nobody decided on.
func TestNothingElseIsServed(t *testing.T) {
	srv := New("127.0.0.1:0", testHandler(sample, true),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("/ = %d, want 404", rec.Code)
	}
}
