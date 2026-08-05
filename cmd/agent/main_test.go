package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/RebuildStackCo/runtime-agent/internal/config"
)

func TestRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	done := make(chan error, 1)
	go func() { done <- run(ctx, logger, fake.NewClientset(), config.Config{}) }()

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
