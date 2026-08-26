package nodeintake

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeScoper struct {
	node string
	pods []string
}

func (f *fakeScoper) AdmittedPodsOnNode(node string) []string {
	f.node = node
	return f.pods
}

func testScopeHandler(t *testing.T, sc NodeScoper) *ScopeHandler {
	t.Helper()
	v := fakeVerifier{accept: goodToken, identity: nodeIdentity()}
	return NewScopeHandler(v, slog.New(slog.NewTextHandler(io.Discard, nil)), sc)
}

func TestScopeHandlerReturnsAdmittedPodsOfTheQueryingNode(t *testing.T) {
	scoper := &fakeScoper{pods: []string{"uid-a", "uid-b"}}
	h := testScopeHandler(t, scoper)

	rec := post(t, h, goodToken, `{"node":"kind-worker"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var resp ScopeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.PodUIDs) != 2 || resp.PodUIDs[0] != "uid-a" {
		t.Errorf("pod uids = %+v, want [uid-a uid-b]", resp.PodUIDs)
	}
	// The answer is scoped to the querying node, never the whole cluster.
	if scoper.node != "kind-worker" {
		t.Errorf("scoper asked for node %q, want kind-worker", scoper.node)
	}
}

// A node with no admitted pods gets an empty answer and scans nothing — the
// same outcome as an unreachable controller, reached honestly.
func TestScopeHandlerReturnsEmptyForNodeWithNoAdmittedPods(t *testing.T) {
	rec := post(t, testScopeHandler(t, &fakeScoper{}), goodToken, `{"node":"`+tokenNode+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp ScopeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.PodUIDs) != 0 {
		t.Errorf("pod uids = %+v, want none", resp.PodUIDs)
	}
}

func TestScopeHandlerRejects(t *testing.T) {
	h := testScopeHandler(t, &fakeScoper{})
	cases := []struct {
		name, token, body string
		want              int
	}{
		{"bad token", "wrong", `{"node":"n"}`, http.StatusUnauthorized},
		{"missing node", goodToken, `{}`, http.StatusBadRequest},
		{"unknown field", goodToken, `{"node":"n","x":1}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := post(t, h, tc.token, tc.body); rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestScopeHandlerRejectsGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, scopePath, nil)
	req.Header.Set("Authorization", "Bearer "+goodToken)
	rec := httptest.NewRecorder()
	testScopeHandler(t, &fakeScoper{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
