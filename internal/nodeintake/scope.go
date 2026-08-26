package nodeintake

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// scopeMaxBodyBytes caps a scope query body. It carries only a node name.
const scopeMaxBodyBytes = 4 << 10 // 4 KiB

// ScopeRequest is a node's scan-scope query, carrying the node's own name.
type ScopeRequest struct {
	Node string `json:"node"`
}

// ScopeResponse is the reply: the UIDs of the pods on the querying node that
// passed the controller's filters. The node scans processes belonging to those
// pods and nothing else.
type ScopeResponse struct {
	PodUIDs []string `json:"pod_uids"`
}

// NodeScoper answers, for a node, which pods it may scan. The controller
// implements it from PodWatcher's index, which contains exactly the pods that
// passed the namespace filters and opt-out annotations.
type NodeScoper interface {
	AdmittedPodsOnNode(node string) []string
}

// ScopeHandler answers a node's scan-scope query.
//
// Why the reply carries data. The node role holds zero Kubernetes API access
// (ADR 0009) and resolves a process only as far as a pod UID and container ID,
// through its cgroup — never to a namespace. It therefore cannot itself honor
// the namespace filters and opt-out annotations that docs/security.md §10.2
// promises are applied on the node, before transport. The controller holds those
// filters, so the node asks it which pods are in scope.
//
// This does not breach invariant 1 (no control channel, ADR 0001), for the same
// reasons as the targets query of ADR 0011 §3: the external backend is not
// involved at any point; the reply is pod identifiers derived from the operator's
// own Helm-values filters applied to the live cluster, never configuration or a
// command; and the reply can only ever *narrow* what the node does. A rogue or
// broken controller can withhold pods from the scope — the node then scans less —
// but naming a pod that the filters exclude is impossible, because the set is
// PodWatcher's admitted index, which excluded pods never enter.
type ScopeHandler struct {
	verifier TokenVerifier
	scoper   NodeScoper
	logger   *slog.Logger
}

// NewScopeHandler builds the scan-scope query handler.
func NewScopeHandler(verifier TokenVerifier, logger *slog.Logger, scoper NodeScoper) *ScopeHandler {
	return &ScopeHandler{verifier: verifier, scoper: scoper, logger: logger}
}

// ServeHTTP authenticates the caller, reads the node name, and returns the pod
// UIDs that node may scan.
func (h *ScopeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		h.logger.Warn("scope query rejected: token verification failed", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req ScopeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, scopeMaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "query too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "malformed query", http.StatusBadRequest)
		return
	}
	if !authorizeNode(w, h.logger, "scope query", identity, req.Node) {
		return
	}

	// identity.Node, not req.Node: the two are equal by the check above, and
	// reading the token's copy is what keeps that true if the check moves.
	resp := ScopeResponse{PodUIDs: h.scoper.AdmittedPodsOnNode(identity.Node)}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
