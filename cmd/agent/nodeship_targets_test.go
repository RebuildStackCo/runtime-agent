package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeintake"
)

func TestTargetsClientFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("auth = %q, want Bearer tok", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nodeintake.TargetsResponse{
			Targets: []nodeintake.Target{{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web"}},
		})
	}))
	t.Cleanup(srv.Close)

	c := newTargetsClient(srv.URL, writeToken(t, "tok"))
	targets, err := c.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].WorkloadName != "web" {
		t.Errorf("targets = %+v, want [web]", targets)
	}
}

func TestTargetsClientNilWhenNoEndpoint(t *testing.T) {
	if newTargetsClient("", "/x") != nil {
		t.Error("expected nil client for empty endpoint")
	}
}

func TestTargetsClientRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c := newTargetsClient(srv.URL, writeToken(t, "tok"))
	if _, err := c.fetch(context.Background()); err == nil {
		t.Error("expected error on non-2xx response")
	}
}
