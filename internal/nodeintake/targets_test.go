package nodeintake

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeauth"
)

type fakeSource struct{ targets []Target }

func (f fakeSource) Snapshot() []Target { return f.targets }

func testTargetsHandler(t *testing.T, src TargetSource) *TargetsHandler {
	t.Helper()
	v := fakeVerifier{accept: goodToken, identity: nodeauth.Identity{Subject: "sub"}}
	return NewTargetsHandler(v, slog.New(slog.NewTextHandler(io.Discard, nil)), src)
}

func TestTargetsHandlerReturnsSnapshot(t *testing.T) {
	src := fakeSource{targets: []Target{{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web"}}}
	rec := post(t, testTargetsHandler(t, src), goodToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var resp TargetsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Targets) != 1 || resp.Targets[0].WorkloadName != "web" {
		t.Errorf("targets = %+v, want [web]", resp.Targets)
	}
}

func TestTargetsHandlerRejectsBadToken(t *testing.T) {
	if rec := post(t, testTargetsHandler(t, fakeSource{}), "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestTargetsHandlerRejectsGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, targetsPath, nil)
	req.Header.Set("Authorization", "Bearer "+goodToken)
	rec := httptest.NewRecorder()
	testTargetsHandler(t, fakeSource{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
