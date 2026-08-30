package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/RebuildStackCo/runtime-agent/internal/config"
)

func TestRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	done := make(chan error, 1)
	// A minimal REST config; node intake is disabled in this config, so it is
	// never used to build a JWKS client.
	go func() {
		done <- run(ctx, logger, fake.NewClientset(), &rest.Config{}, config.Config{}, config.Shape{}, time.Now())
	}()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error on graceful stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop after context cancellation")
	}
}
