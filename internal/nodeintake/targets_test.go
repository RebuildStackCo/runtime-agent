package nodeintake

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeTargeter struct{ containers []string }

func (f fakeTargeter) ContainersForNode(string) []string { return f.containers }

func testTargetsHandler(t *testing.T, tg NodeTargeter) *TargetsHandler {
	t.Helper()
	v := fakeVerifier{accept: goodToken, identity: nodeIdentity()}
	return NewTargetsHandler(v, slog.New(slog.NewTextHandler(io.Discard, nil)), tg)
}

func TestTargetsHandlerReturnsContainers(t *testing.T) {
	h := testTargetsHandler(t, fakeTargeter{containers: []string{"abc", "def"}})
	rec := post(t, h, goodToken, `{"node":"kind-worker"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var resp TargetsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.ContainerIDs) != 2 || resp.ContainerIDs[0] != "abc" {
		t.Errorf("container ids = %+v, want [abc def]", resp.ContainerIDs)
	}
}

func TestTargetsHandlerRejects(t *testing.T) {
	h := testTargetsHandler(t, fakeTargeter{})
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

func TestTargetsHandlerRejectsGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, targetsPath, nil)
	req.Header.Set("Authorization", "Bearer "+goodToken)
	rec := httptest.NewRecorder()
	testTargetsHandler(t, fakeTargeter{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
