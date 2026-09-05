// Package health serves the two questions the kubelet asks a pod about itself:
// is this process still working, and may it be depended on yet.
//
// It is one listener per role, on a port of its own, and it answers nothing
// else: no request body is read, no query is parsed, and no reply names a
// cluster object (ADR 0069, CLAUDE.md invariants 1 and 6).
package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// The two paths, in the API server's vocabulary, because the distinction is the
// whole point: /livez is the process, /readyz is what it has collected.
const (
	LivePath  = "/livez"
	ReadyPath = "/readyz"
)

// Check answers one question. The string is why the answer is no, and it may
// name only the agent's own machinery — a resource class, a component — never
// anything read from the cluster.
type Check func() (ok bool, reason string)

// Server runs the health listener until its context is canceled. It is one more
// long-lived task in each role's lifecycle.
type Server struct {
	addr    string
	handler http.Handler
	logger  *slog.Logger
}

// New wires a listener on addr answering live at /livez and ready at /readyz.
func New(addr string, live, ready Check, logger *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.Handle(LivePath, answer(live))
	mux.Handle(ReadyPath, answer(ready))
	return &Server{addr: addr, handler: mux, logger: logger}
}

// answer turns a check into a handler that reads nothing from the request but
// its method. A method other than GET or HEAD is refused rather than treated as
// a read: there is no request this endpoint acts on (CLAUDE.md invariant 1).
func answer(check Check) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "read-only endpoint", http.StatusMethodNotAllowed)
			return
		}
		ok, reason := check()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// The status code is the answer; the line is for whoever reads a
		// `kubectl describe`. A write that fails has nobody left to tell.
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, reason+"\n")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
}

// Run serves until ctx is canceled, then shuts down. A listener that cannot
// bind is a startup failure: probes that cannot be answered restart the pod
// anyway, and the reason should be in the agent's own log.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		s.logger.Info("health listener serving", "addr", s.addr,
			"live", LivePath, "ready", ReadyPath)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("health listener shutdown: %w", err)
		}
		s.logger.Info("health listener stopped")
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("health listener serve: %w", err)
	}
}
