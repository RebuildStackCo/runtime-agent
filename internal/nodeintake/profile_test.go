package nodeintake

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeauth"
)

func testProfileHandler(t *testing.T, onProfile func(nodeauth.Identity, ProfileReport)) *ProfileHandler {
	t.Helper()
	v := fakeVerifier{accept: goodToken, identity: nodeauth.Identity{
		Subject:        "system:serviceaccount:runtime-agent:runtime-agent-node",
		ServiceAccount: "runtime-agent-node",
	}}
	return NewProfileHandler(v, slog.New(slog.NewTextHandler(io.Discard, nil)), onProfile)
}

// base64 "AQID" decodes to 3 bytes {1,2,3}.
const validProfileBody = `{"node":"kind-worker","pod_uid":"abc","container_id":"def","pid":42,"capture_start":"2023-11-14T22:13:20Z","capture_end":"2023-11-14T22:14:20Z","pprof":"AQID"}`

func TestProfileHandlerAcceptsValid(t *testing.T) {
	var got *ProfileReport
	var gotID nodeauth.Identity
	h := testProfileHandler(t, func(id nodeauth.Identity, r ProfileReport) { got, gotID = &r, id })

	rec := post(t, h, goodToken, validProfileBody)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}
	if got == nil || got.PodUID != "abc" || got.ContainerID != "def" || got.PID != 42 {
		t.Fatalf("decoded report wrong: %+v", got)
	}
	if len(got.Pprof) != 3 {
		t.Errorf("pprof bytes = %d, want 3", len(got.Pprof))
	}
	if gotID.ServiceAccount != "runtime-agent-node" {
		t.Errorf("caller identity not passed through: %+v", gotID)
	}
}

func TestProfileHandlerRejects(t *testing.T) {
	h := testProfileHandler(t, func(nodeauth.Identity, ProfileReport) {})
	cases := []struct {
		name, token, body string
		want              int
	}{
		{"bad token", "wrong", validProfileBody, http.StatusUnauthorized},
		{"missing token", "", validProfileBody, http.StatusUnauthorized},
		{"unknown field", goodToken, `{"node":"x","surprise":1}`, http.StatusBadRequest},
		{"trailing data", goodToken, validProfileBody + `{}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := post(t, h, tc.token, tc.body); rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
