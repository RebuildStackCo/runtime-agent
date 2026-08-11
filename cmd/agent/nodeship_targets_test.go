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
	var gotReq nodeintake.TargetsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("auth = %q, want Bearer tok", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nodeintake.TargetsResponse{ContainerIDs: []string{"abc", "def"}})
	}))
	t.Cleanup(srv.Close)

	c := newTargetsClient(srv.URL, writeToken(t, "tok"))
	containers, err := c.fetch(context.Background(), "kind-worker")
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.Node != "kind-worker" {
		t.Errorf("query node = %q, want kind-worker", gotReq.Node)
	}
	if len(containers) != 2 || containers[0] != "abc" {
		t.Errorf("containers = %+v, want [abc def]", containers)
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
	if _, err := c.fetch(context.Background(), "node"); err == nil {
		t.Error("expected error on non-2xx response")
	}
}
