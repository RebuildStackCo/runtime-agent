package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeintake"
)

func TestScopeClientFetch(t *testing.T) {
	var gotReq nodeintake.ScopeRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("auth = %q, want Bearer tok", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nodeintake.ScopeResponse{PodUIDs: []string{"uid-a", "uid-b"}})
	}))
	t.Cleanup(srv.Close)

	c := newScopeClient(srv.URL, writeToken(t, "tok"))
	uids, err := c.fetch(context.Background(), "kind-worker")
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.Node != "kind-worker" {
		t.Errorf("query node = %q, want kind-worker", gotReq.Node)
	}
	if len(uids) != 2 || uids[0] != "uid-a" {
		t.Errorf("pod uids = %+v, want [uid-a uid-b]", uids)
	}
}

func TestScopeClientNilWhenNoEndpoint(t *testing.T) {
	if newScopeClient("", "/x") != nil {
		t.Error("expected nil client for empty endpoint")
	}
}

func TestScopeClientRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := newScopeClient(srv.URL, writeToken(t, "tok"))
	if _, err := c.fetch(context.Background(), "n"); err == nil {
		t.Error("expected an error for a non-2xx scope reply")
	}
}

// runNodeOnce runs the node role for a single pass and returns its log output.
func runNodeOnce(t *testing.T, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	done := make(chan error, 1)
	go func() {
		done <- runNode(context.Background(), logger, append([]string{"-proc", t.TempDir(), "-interval", "0"}, args...))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runNode returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runNode single pass did not return")
	}
	return buf.String()
}

// With no scope endpoint the node cannot know which pods passed the customer's
// filters, so it must scan nothing rather than scan everything (ADR 0015). The
// warning is part of the behavior: an operator who misconfigures the flag must
// be able to see why no inventory appears.
func TestRunNodeWithoutScopeEndpointScansNothing(t *testing.T) {
	out := runNodeOnce(t)

	if !strings.Contains(out, "no scope endpoint configured") {
		t.Errorf("log does not explain the empty scope:\n%s", out)
	}
	if !strings.Contains(out, "pods_in_scope=0") {
		t.Errorf("log does not report an empty scope:\n%s", out)
	}
}

// The scope the controller answers with is what the scanner is given: the flag,
// the client, and the scanner are wired end to end.
func TestRunNodeUsesTheControllerScope(t *testing.T) {
	var gotNode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req nodeintake.ScopeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotNode = req.Node
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nodeintake.ScopeResponse{PodUIDs: []string{"uid-a", "uid-b", "uid-c"}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("NODE_NAME", "kind-worker")

	out := runNodeOnce(t, "-scope-endpoint", srv.URL, "-token-path", writeToken(t, "tok"))

	if gotNode != "kind-worker" {
		t.Errorf("controller was asked for node %q, want kind-worker", gotNode)
	}
	if !strings.Contains(out, "pods_in_scope=3") {
		t.Errorf("scanner did not receive the controller's scope:\n%s", out)
	}
}

// A controller that cannot be reached fails closed, not open: one pass is lost,
// which the next recovers (loss-harmless), and no process is scanned meanwhile.
func TestRunNodeFailsClosedWhenScopeQueryFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	out := runNodeOnce(t, "-scope-endpoint", srv.URL, "-token-path", writeToken(t, "tok"))

	if !strings.Contains(out, "fetching scan scope failed") {
		t.Errorf("log does not report the failed scope query:\n%s", out)
	}
	if !strings.Contains(out, "pods_in_scope=0") {
		t.Errorf("a failed scope query must leave the scope empty:\n%s", out)
	}
}
