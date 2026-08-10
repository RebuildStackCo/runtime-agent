package nodeintake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// The receiver's path. Nodes POST their reports here.
const reportPath = "/v1/node-inventory"

// Server runs the node-intake HTTP receiver until its context is canceled.
// It is one more long-lived task in the controller's lifecycle, alongside the
// watchers and the usage poller.
type Server struct {
	addr    string
	handler http.Handler
	logger  *slog.Logger
}

// NewServer wires a receiver listening on addr that dispatches the report path
// to handler. addr follows net/http conventions (e.g. ":8080").
func NewServer(addr string, handler http.Handler, logger *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.Handle(reportPath, handler)
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
