// Command agent is the RebuildStack runtime agent. It collects resource-usage
// rollups and workload metadata inside a Kubernetes cluster and ships them
// one-way to a backend. See docs/ for the architecture.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	err := run(ctx, logger)
	stop()
	if err != nil {
		logger.Error("agent exited with error", "error", err)
		os.Exit(1)
	}
}

// run is the agent's lifecycle: it starts, works until ctx is canceled, and
// returns. Collectors and sinks will be wired in here.
//
//nolint:unparam // the error return is the contract; collectors wired in later can fail
func run(ctx context.Context, logger *slog.Logger) error {
	logger.Info("agent starting", "version", version)
	<-ctx.Done()
	logger.Info("agent stopping")
	return nil
}
