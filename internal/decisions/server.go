// Package decisions answers, inside the cluster and only inside it, which
// objects the agent's filters admitted and which they refused — by name, with
// the reason (ADR 0084).
//
// It is the one surface of this agent that names an object the filters
// excluded. That is why it is a listener of its own, bound to loopback, and
// why nothing here is reachable from anywhere a payload could be built: the
// names answered here never leave the cluster (CLAUDE.md invariant 6).
package decisions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

// Path is where the list is served.
const Path = "/collection"

// Handler answers the list as newline-delimited JSON, one object per line, so
// a reader can process a large cluster's answer as it arrives rather than
// waiting for one document to be built and parsed.
//
// It reads nothing from the request but its method: no body, no query, no
// header. A filter is `jq`'s job, and a parameter here would be a request this
// endpoint acts on (CLAUDE.md invariant 1).
func Handler(list func() []collector.Decision, synced func() bool, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "read-only endpoint", http.StatusMethodNotAllowed)
			return
		}
		// An unsynced cache would answer a short list that looks like a
		// complete one, which is the failure this endpoint exists to prevent.
		if !synced() {
			http.Error(w, "informer caches are still syncing", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		enc := json.NewEncoder(w)
		for _, d := range list() {
			if err := enc.Encode(d); err != nil {
				// The status line is already sent, so the reader learns of the
				// truncation from the stream ending early.
				logger.Warn("collection listing cut short", "error", err)
				return
			}
		}
	})
}

// Server runs the listener until its context is canceled: one more long-lived
// task in the controller's lifecycle, beside the health listener rather than
// on it.
type Server struct {
	addr    string
	handler http.Handler
	logger  *slog.Logger
}

// New wires a listener on addr serving the list at Path and nothing else.
func New(addr string, h http.Handler, logger *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.Handle(Path, h)
	return &Server{addr: addr, handler: mux, logger: logger}
}

// Run serves until ctx is canceled, then shuts down. A listener that cannot
// bind is a startup failure, as the health listener's is: an endpoint the
// operator configured and that silently does not exist is worse than a refusal
// to start.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		// Long enough to write a large cluster's list to a reader that is
		// paging it through a port-forward.
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		s.logger.Info("collection listener serving", "addr", s.addr, "path", Path)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("collection listener shutdown: %w", err)
		}
		s.logger.Info("collection listener stopped")
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("collection listener serve: %w", err)
	}
}
