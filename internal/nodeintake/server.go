package nodeintake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// The receiver paths. Nodes POST their inventory reports to reportPath and their
// captured profiles to profilePath (ADR 0010, ADR 0011 §5.5).
const (
	reportPath  = "/v1/node-inventory"
	profilePath = "/v1/node-profile"
	targetsPath = "/v1/node-targets"
)

// Server runs the node-intake HTTP receiver until its context is canceled.
// It is one more long-lived task in the controller's lifecycle, alongside the
// watchers and the usage poller.
type Server struct {
	addr    string
	handler http.Handler
	logger  *slog.Logger
}

// NewServer wires a receiver listening on addr. It always dispatches the
// inventory path to inventory, and mounts the profile and targets paths when
// their handlers are non-nil. addr follows net/http conventions (e.g. ":8080").
// All endpoints share the one port, the one token audience, and the one
// NetworkPolicy. The inventory and profile endpoints are one-way pushes (the
// reply is an ack); the targets endpoint is the config-bounded query whose reply
// carries data (ADR 0011 §3).
func NewServer(addr string, inventory, profile, targets http.Handler, logger *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.Handle(reportPath, inventory)
	if profile != nil {
		mux.Handle(profilePath, profile)
	}
	if targets != nil {
		mux.Handle(targetsPath, targets)
	}
	return &Server{addr: addr, handler: mux, logger: logger}
}

// Run serves until ctx is canceled, then shuts down gracefully. It returns nil
// on a clean shutdown and an error only if the listener fails to start or serve
// for a reason other than the shutdown.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	serveErr := make(chan error, 1)
	go func() {
		s.logger.Info("node intake receiver listening", "addr", s.addr, "path", reportPath)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("node intake shutdown: %w", err)
		}
		s.logger.Info("node intake receiver stopped")
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("node intake serve: %w", err)
	}
}
