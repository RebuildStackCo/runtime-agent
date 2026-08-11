package nodeintake

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Target names one workload the controller suggests the node profile. It carries
// identifiers only — nothing the node executes.
type Target struct {
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workload_kind"`
	WorkloadName string `json:"workload_name"`
}

// TargetsResponse is the reply to a node's targets query.
type TargetsResponse struct {
	Targets []Target `json:"targets"`
}

// TargetSource yields the currently published top-N targets. The controller's
// publisher implements it by reading an atomically published snapshot — never
// the live rollup state (ADR 0011 §6b concurrency note).
type TargetSource interface {
	Snapshot() []Target
}

// TargetsHandler answers a node's targets query with the top-N workloads by
// consumption.
//
// This is the one node↔controller endpoint whose reply carries data the node
// acts on, and it is a deliberate, config-bounded exception to the one-way reply
// discipline of ADR 0010 §1 (ADR 0011 §3). It does not break invariant 1: the
// reply is data derived from the cluster's own rollups (workload identifiers),
// never configuration or a command; the external backend is untouched; and the
// node's ConfigMap — not this reply — bounds what may be profiled and how much.
// The published snapshot is already filtered to the eligible set and capped at
// TopN, and the node re-checks eligibility before it acts. The worst a rogue
// controller can do is reorder already-permitted targets within already-set
// ceilings.
type TargetsHandler struct {
	verifier TokenVerifier
	source   TargetSource
	logger   *slog.Logger
}

// NewTargetsHandler builds the targets query handler.
func NewTargetsHandler(verifier TokenVerifier, logger *slog.Logger, source TargetSource) *TargetsHandler {
	return &TargetsHandler{verifier: verifier, source: source, logger: logger}
}

// ServeHTTP authenticates the caller and returns the published targets. The
// query is parameterless — the top-N is cluster-wide — so no request body is
// read; the node intersects the reply with its own pods locally.
func (h *TargetsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if _, err := h.verifier.Verify(r.Context(), token); err != nil {
		h.logger.Warn("targets query rejected: token verification failed", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	resp := TargetsResponse{Targets: h.source.Snapshot()}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
