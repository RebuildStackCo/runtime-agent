package nodeintake

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"github.com/RebuildStackCo/runtime-agent/internal/nodeauth"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// fakeVerifier accepts one fixed token and rejects everything else.
type fakeVerifier struct {
	accept   string
	identity nodeauth.Identity
}

func (f fakeVerifier) Verify(_ context.Context, token string) (nodeauth.Identity, error) {
	if token != f.accept {
		return nodeauth.Identity{}, errors.New("invalid token")
	}
	return f.identity, nil
}

const (
	goodToken = "good-token"
	// tokenNode is the node the accepted token establishes. Every request body
	// in these tests must name it, because a caller may only speak for the node
	// its token says it runs on (ADR 0040).
	tokenNode = "kind-worker"
)

// nodeIdentity is what a verified node token yields: one shared role subject,
// and the node claim that distinguishes one DaemonSet pod from another.
func nodeIdentity() nodeauth.Identity {
	return nodeauth.Identity{
		Subject:        "system:serviceaccount:runtime-agent:runtime-agent-node",
		Namespace:      "runtime-agent",
		ServiceAccount: "runtime-agent-node",
		Node:           tokenNode,
	}
}

func testHandler(t *testing.T, onReport func(nodeauth.Identity, nodescan.Report)) *Handler {
	t.Helper()
	v := fakeVerifier{accept: goodToken, identity: nodeIdentity()}
	return NewHandler(v, slog.New(slog.NewTextHandler(io.Discard, nil)), onReport)
}

func post(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, reportPath, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const validBody = `{"node":"kind-worker","binaries":[{"pid":42,"go_version":"go1.26.1","main_module":"example.com/app","pgo":true,"pod_uid":"abc","container_id":"def"}],"counters":{"processes_scanned":10,"go_found":1,"filtered_infra":3,"unreadable":2}}`

func TestHandlerAcceptsValidReport(t *testing.T) {
	var got *nodescan.Report
	var gotID nodeauth.Identity
	h := testHandler(t, func(id nodeauth.Identity, r nodescan.Report) {
		gotID = id
		got = &r
	})

	rec := post(t, h, goodToken, validBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body %q", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if got == nil {
		t.Fatal("onReport not invoked")
	}
	if got.Node != "kind-worker" || len(got.Binaries) != 1 {
		t.Errorf("decoded report = %+v, want node kind-worker with 1 binary", got)
	}
	if got.Binaries[0].MainModule != "example.com/app" || !got.Binaries[0].PGO {
		t.Errorf("decoded binary = %+v", got.Binaries[0])
	}
	if got.Counters.FilteredInfra != 3 || got.Counters.Unreadable != 2 {
		t.Errorf("decoded counters = %+v", got.Counters)
	}
	if gotID.ServiceAccount != "runtime-agent-node" {
		t.Errorf("identity = %+v, want the node ServiceAccount", gotID)
	}
}

func TestHandlerRejectsMissingToken(t *testing.T) {
	called := false
	h := testHandler(t, func(nodeauth.Identity, nodescan.Report) { called = true })
	rec := post(t, h, "", validBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("onReport invoked for an unauthenticated request")
	}
}

func TestHandlerRejectsBadToken(t *testing.T) {
	called := false
	h := testHandler(t, func(nodeauth.Identity, nodescan.Report) { called = true })
	rec := post(t, h, "wrong-token", validBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("onReport invoked for a rejected token")
	}
}

func TestHandlerRejectsMalformedBody(t *testing.T) {
	h := testHandler(t, func(nodeauth.Identity, nodescan.Report) {
		t.Error("onReport invoked for a malformed body")
	})
	rec := post(t, h, goodToken, `{"node": "x", not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandlerRejectsUnknownFields(t *testing.T) {
	h := testHandler(t, func(nodeauth.Identity, nodescan.Report) {
		t.Error("onReport invoked for a body with unknown fields")
	})
	rec := post(t, h, goodToken, `{"node":"x","surprise":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field rejected)", rec.Code)
	}
}

func TestHandlerRejectsTrailingData(t *testing.T) {
	h := testHandler(t, func(nodeauth.Identity, nodescan.Report) {
		t.Error("onReport invoked for a body with trailing data")
	})
	rec := post(t, h, goodToken, validBody+`{"another":"object"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (trailing data rejected)", rec.Code)
	}
}

func TestHandlerRejectsOversizeBody(t *testing.T) {
	h := testHandler(t, func(nodeauth.Identity, nodescan.Report) {
		t.Error("onReport invoked for an oversize body")
	})
	h.maxBodyBytes = 64 // tiny cap for the test
	rec := post(t, h, goodToken, validBody)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestHandlerRejectsWrongMethod(t *testing.T) {
	h := testHandler(t, nil)
	req := httptest.NewRequest(http.MethodGet, reportPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow header = %q, want POST", allow)
	}
}

func TestHandlerNilOnReportStillAccepts(t *testing.T) {
	h := NewHandler(fakeVerifier{accept: goodToken, identity: nodeIdentity()},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	rec := post(t, h, goodToken, validBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 with a nil onReport", rec.Code)
	}
}

// A refused report is data that left a node and reached nothing, and the only
// other place it appears is that node's own log, in the customer's cluster
// (ADR 0067). Every refusal path is counted, by the reason the caller was given.
func TestEveryRefusalIsCountedByItsReason(t *testing.T) {
	h := testHandler(t, nil)
	h.maxBodyBytes = 64

	big := `{"node":"` + tokenNode + `","binaries":[` + strings.Repeat(`{"pid":1},`, 40) + `{"pid":2}]}`
	for _, tc := range []struct {
		name, token, body string
		want              int
	}{
		{"no token", "", `{"node":"` + tokenNode + `"}`, http.StatusUnauthorized},
		{"bad token", "not-the-token", `{"node":"` + tokenNode + `"}`, http.StatusUnauthorized},
		{"too large", goodToken, big, http.StatusRequestEntityTooLarge},
		{"malformed", goodToken, `{"node":`, http.StatusBadRequest},
		{"no node", goodToken, `{"binaries":[]}`, http.StatusBadRequest},
		{"another node", goodToken, `{"node":"someone-else"}`, http.StatusForbidden},
	} {
		if got := post(t, h, tc.token, tc.body).Code; got != tc.want {
			t.Fatalf("%s: status = %d, want %d", tc.name, got, tc.want)
		}
	}

	got := h.Rejections()
	want := model.IntakeRejections{Unauthorized: 3, TooLarge: 1, Malformed: 2}
	if got != want {
		t.Errorf("rejections = %+v, want %+v", got, want)
	}
}

// An accepted report moves nothing: the counters are what was refused, not what
// arrived.
func TestAnAcceptedReportCountsNoRejection(t *testing.T) {
	h := testHandler(t, func(nodeauth.Identity, nodescan.Report) {})
	if got := post(t, h, goodToken, `{"node":"`+tokenNode+`","binaries":[]}`).Code; got != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", got, http.StatusAccepted)
	}
	if got := h.Rejections(); got != (model.IntakeRejections{}) {
		t.Errorf("rejections = %+v, want all zero", got)
	}
}
