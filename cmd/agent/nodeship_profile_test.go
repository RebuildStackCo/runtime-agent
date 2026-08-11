package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeintake"
)

func TestProfileShipperPostsProfile(t *testing.T) {
	var mu sync.Mutex
	var gotAuth, gotMethod string
	var gotReport nodeintake.ProfileReport
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits++
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotReport)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	sh := newProfileShipper(srv.URL, writeToken(t, "tok-123"))
	report := nodeintake.ProfileReport{
		Node: "n", PodUID: "puid", ContainerID: "cid", PID: 7,
		CaptureStart: time.Unix(100, 0).UTC(), CaptureEnd: time.Unix(160, 0).UTC(),
		Pprof: []byte("PPROF-BYTES"),
	}
	if err := sh.ship(context.Background(), report); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 || gotMethod != http.MethodPost {
		t.Errorf("hits=%d method=%s, want 1 POST", hits, gotMethod)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("auth = %q, want Bearer tok-123", gotAuth)
	}
	if gotReport.PodUID != "puid" || gotReport.ContainerID != "cid" || string(gotReport.Pprof) != "PPROF-BYTES" {
		t.Errorf("decoded report wrong: %+v", gotReport)
	}
}

func TestProfileShipperNilWhenNoEndpoint(t *testing.T) {
	if newProfileShipper("", "/x") != nil {
		t.Error("expected nil shipper for empty endpoint (capture-only mode)")
	}
}

func TestProfileShipperRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	sh := newProfileShipper(srv.URL, writeToken(t, "tok"))
	if err := sh.ship(context.Background(), nodeintake.ProfileReport{}); err == nil {
		t.Error("expected error on non-2xx response")
	}
}
