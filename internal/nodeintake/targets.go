package nodeintake

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// targetsMaxBodyBytes caps a targets query body. It carries only a node name.
const targetsMaxBodyBytes = 4 << 10 // 4 KiB

// TargetsRequest is a node's targets query. It carries the node's own name so
// the controller can scope the answer to containers on that node — the node
// cannot resolve a container to a workload itself (no API access, ADR 0009).
type TargetsRequest struct {
	Node string `json:"node"`
}

// TargetsResponse is the reply: the runtime container IDs on the querying node
// that belong to the top-N eligible workloads. These are node-actionable — the
// node profiles the processes whose cgroup container ID is in this set.
type TargetsResponse struct {
	ContainerIDs []string `json:"container_ids"`
}

// NodeTargeter answers, for a node, which container IDs it should profile. The
// controller implements it by expanding the published top-N workloads to the
// containers of their pods on that node (via PodWatcher).
type NodeTargeter interface {
	ContainersForNode(node string) []string
}

// TargetsHandler answers a node's targets query with the container IDs to
// profile on that node.
//
// This is the one node↔controller endpoint whose reply carries data the node
// acts on — a deliberate, config-bounded exception to the one-way reply
// discipline of ADR 0010 §1 (ADR 0011 §3). It does not break invariant 1: the
// reply is container identifiers derived from the cluster's own rollups and
// PodWatcher, never configuration or a command; the external backend is
// untouched; and the operator's ConfigMap — the eligible set the publisher
// filters to — bounds what may be named. Eligibility is enforced on the
// controller (here and again at the ship-time join, ADR 0011 §5.5), because the
// node has no API to check it. The worst a rogue controller can do is reorder
// already-permitted targets.
type TargetsHandler struct {
	verifier TokenVerifier
	targeter NodeTargeter
	logger   *slog.Logger
}

// NewTargetsHandler builds the targets query handler.
func NewTargetsHandler(verifier TokenVerifier, logger *slog.Logger, targeter NodeTargeter) *TargetsHandler {
	return &TargetsHandler{verifier: verifier, targeter: targeter, logger: logger}
}

// ServeHTTP authenticates the caller, reads the node name, and returns the
// container IDs to profile on that node.
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

	var req TargetsRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, targetsMaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "query too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "malformed query", http.StatusBadRequest)
		return
	}
	if req.Node == "" {
		http.Error(w, "node required", http.StatusBadRequest)
		return
	}

	resp := TargetsResponse{ContainerIDs: h.targeter.ContainersForNode(req.Node)}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
