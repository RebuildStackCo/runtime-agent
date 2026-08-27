package nodeintake

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeauth"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// postTo is post() for a handler mounted at its own path. The existing helper
// hardcodes the inventory path, which every handler tolerates in a unit test —
// but a case that reads as "POST /v1/node-scope" should be one.
func postTo(t *testing.T, h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestNoEndpointLetsANodeSpeakForAnotherNode is ADR 0040 as a test, across all
// four endpoints at once, because the property is worth nothing unless it holds
// on every one: a token establishes which node is calling, and a caller naming
// another node is refused.
//
// Each case is written as the attack rather than as the mechanism, so a later
// refactor that drops the check fails with a description of what it permitted.
func TestNoEndpointLetsANodeSpeakForAnotherNode(t *testing.T) {
	const foreign = "some-other-node"
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	verifier := fakeVerifier{accept: goodToken, identity: nodeIdentity()}

	// A body identical to each endpoint's valid one, but naming a node the
	// caller's token does not.
	inventory := replaceNode(t, validBody, foreign)
	profile := replaceNode(t, validProfileBody, foreign)

	cases := []struct {
		name    string
		handler http.Handler
		path    string
		body    string
		attack  string
	}{
		{
			name:    "inventory",
			handler: NewHandler(verifier, discard, func(_ nodeauth.Identity, _ nodescan.Report) {}),
			path:    reportPath,
			body:    inventory,
			attack:  "file Go-inventory facts against a workload on another node",
		},
		{
			name:    "profile",
			handler: NewProfileHandler(verifier, discard, func(_ nodeauth.Identity, _ ProfileReport) {}),
			path:    profilePath,
			body:    profile,
			attack:  "file a captured profile against a workload on another node",
		},
		{
			name:    "scope",
			handler: NewScopeHandler(verifier, discard, &fakeScoper{pods: []string{"uid-a"}}),
			path:    scopePath,
			body:    `{"node":"` + foreign + `"}`,
			attack:  "read which pods the controller admitted on another node",
		},
		{
			name:    "targets",
			handler: NewTargetsHandler(verifier, discard, fakeTargeter{containers: []string{"abc"}}),
			path:    targetsPath,
			body:    `{"node":"` + foreign + `"}`,
			attack:  "read which containers are profiling targets on another node",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postTo(t, c.handler, c.path, goodToken, c.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 — a valid token could %s; body %s",
					rec.Code, c.attack, rec.Body.String())
			}
			// The refusal must not leak the answer it declined to give.
			if body := rec.Body.String(); len(body) > 64 {
				t.Errorf("refusal body is %d bytes; a refusal says no and nothing else: %q", len(body), body)
			}
		})
	}
}

// TestAnEmptyNodeIsRefusedRatherThanMatchedAgainstNothing covers the shape a
// buggy client is likeliest to produce. It must not be mistaken for a wildcard.
func TestAnEmptyNodeIsRefusedRatherThanMatchedAgainstNothing(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewScopeHandler(fakeVerifier{accept: goodToken, identity: nodeIdentity()},
		discard, &fakeScoper{pods: []string{"uid-a"}})

	rec := postTo(t, h, scopePath, goodToken, `{"node":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an empty node", rec.Code)
	}
}

// TestTheAnswerIsScopedToTheTokensNodeNotTheBodys pins which of the two equal
// strings is actually used. They agree by the time the answer is built, so a
// regression here is invisible until the check is loosened — at which point the
// body would decide again.
func TestTheAnswerIsScopedToTheTokensNodeNotTheBodys(t *testing.T) {
	scoper := &fakeScoper{pods: []string{"uid-a"}}
	h := NewScopeHandler(fakeVerifier{accept: goodToken, identity: nodeIdentity()},
		slog.New(slog.NewTextHandler(io.Discard, nil)), scoper)

	if rec := postTo(t, h, scopePath, goodToken, `{"node":"`+tokenNode+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if scoper.node != tokenNode {
		t.Errorf("scoper asked for node %q, want the token's %q", scoper.node, tokenNode)
	}
}

// replaceNode rewrites the "node" field of a JSON body, so each case above
// differs from its endpoint's known-good body in exactly one field.
func replaceNode(t *testing.T, body, node string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("test body is not JSON: %v", err)
	}
	m["node"] = node
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
