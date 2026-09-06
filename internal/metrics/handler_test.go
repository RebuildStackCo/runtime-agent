package metrics

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ask(t *testing.T, h http.Handler, method string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, MetricsPathForTest, nil))
	return rec
}

// MetricsPathForTest keeps this package from importing the health package for
// one string; the path itself is health's to define.
const MetricsPathForTest = "/metrics"

func TestTheExpositionIsTheTextFormat(t *testing.T) {
	s := NewSet()
	s.Counter("pods_observed_total", "Pod appearances the filter considered.", 412)
	s.Gauge("spool_files", "Payload files held.", 12, L(LabelRole, RoleController))
	h := Handler(s.Families, discardLogger())

	rec := ask(t, h, http.MethodGet)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# HELP runtime_agent_pods_observed_total Pod appearances the filter considered.",
		"# TYPE runtime_agent_pods_observed_total counter",
		"runtime_agent_pods_observed_total 412",
		"# TYPE runtime_agent_spool_files gauge",
		`runtime_agent_spool_files{role="controller"} 12`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition is missing %q:\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type is %q", ct)
	}
}

// TestOnlyGETAndHEADAreAnswered: nothing a caller sends changes what the agent
// does, and this endpoint is no exception (CLAUDE.md invariant 1).
func TestOnlyGETAndHEADAreAnswered(t *testing.T) {
	h := Handler(NewSet().Families, discardLogger())
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := ask(t, h, method)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s answered with %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("%s: Allow is %q", method, allow)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if rec := ask(t, h, method); rec.Code != http.StatusOK {
			t.Errorf("%s answered with %d, want 200", method, rec.Code)
		}
	}
}

// TestAGatherFailureServesNothing: a refused label name must not reach a
// scraper, and the reason belongs in the agent's log rather than in the reply.
func TestAGatherFailureServesNothing(t *testing.T) {
	// The one way here: a label name outside the permitted set.
	bad := NewSet()
	bad.Gauge("pods_observed", "help", 1, L(LabelName("namespace"), "kube-system"))
	h := Handler(bad.Families, discardLogger())
	rec := ask(t, h, http.MethodGet)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "namespace") || strings.Contains(body, "kube-system") {
		t.Errorf("the refusal repeated what it refused: %q", body)
	}
}
