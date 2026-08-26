// Package nodeintake is the controller's receiver for the node→controller
// inventory channel (ADR 0010). The node initiates every connection and pushes
// its already-filtered scan facts here; the receiver authenticates the caller
// with a projected ServiceAccount token verified locally (nodeauth), decodes
// the report, and hands it to a callback. The response carries only an
// acknowledgment or an error — never configuration or commands: this ingress is
// one-way, mirroring the agent↔backend contract one level down (ADR 0001).
package nodeintake

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeauth"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// defaultMaxBodyBytes caps a single node report. A node's kept-binary list is
// small (customer Go workloads on one node), so this is generous headroom, not
// a tuned limit; its purpose is to refuse a runaway or hostile body outright.
const defaultMaxBodyBytes = 4 << 20 // 4 MiB

// TokenVerifier validates a bearer token and returns the caller's identity.
// *nodeauth.Verifier satisfies it; tests supply a fake.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (nodeauth.Identity, error)
}

// Handler authenticates and decodes node reports, then invokes onReport with
// the verified caller identity and the report. onReport must not block the
// request goroutine for long; it does the cheap in-memory join in this design.
type Handler struct {
	verifier     TokenVerifier
	onReport     func(nodeauth.Identity, nodescan.Report)
	logger       *slog.Logger
	maxBodyBytes int64
}

// NewHandler builds a receiver handler. onReport may be nil (the report is
// authenticated and decoded, then dropped — useful before the join exists).
func NewHandler(verifier TokenVerifier, logger *slog.Logger, onReport func(nodeauth.Identity, nodescan.Report)) *Handler {
	return &Handler{
		verifier:     verifier,
		onReport:     onReport,
		logger:       logger,
		maxBodyBytes: defaultMaxBodyBytes,
	}
}

// ServeHTTP implements the receiver. It accepts POST with a bearer token,
// verifies it, decodes a nodescan.Report, and acknowledges. Every failure path
// returns a status and a short reason; no failure reveals whether a token was
// well-formed-but-untrusted versus malformed beyond the coarse 401.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token, ok := bearerToken(r)
	if !ok {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	identity, err := h.verifier.Verify(r.Context(), token)
	if err != nil {
		// Do not echo the verifier's reason to the caller; log it for the
		// operator instead. A caller learns only "unauthorized".
		h.logger.Warn("node report rejected: token verification failed", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var report nodescan.Report
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "report too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "malformed report", http.StatusBadRequest)
		return
	}
	// Reject trailing data after the JSON object — a well-formed report is a
	// single object, nothing more.
	if dec.More() {
		http.Error(w, "malformed report", http.StatusBadRequest)
		return
	}
	if !authorizeNode(w, h.logger, "node report", identity, report.Node) {
		return
	}

	if h.onReport != nil {
		h.onReport(identity, report)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(struct {
		Status string `json:"status"`
	}{Status: "accepted"})
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header, case-insensitively on the scheme.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const scheme = "bearer "
	if len(h) < len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(h[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}
