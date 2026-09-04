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
// that belong to the top-N collected workloads. These are node-actionable — the
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
// The one endpoint whose reply carries data the node acts on: a config-bounded
// exception to ADR 0010 §1, not a control channel, since the reply is container
// identifiers from the cluster's own rollups (ADR 0011 §3). The bound is the
// collection filter — an excluded workload was never measured, so it is in
// neither the rollups nor the admitted index (ADR 0025).
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
	identity, err := h.verifier.Verify(r.Context(), token)
	if err != nil {
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
	if !authorizeNode(w, h.logger, nil, "targets query", identity, req.Node) {
		return
	}

	resp := TargetsResponse{ContainerIDs: h.targeter.ContainersForNode(identity.Node)}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
