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
// The node holds zero API access (ADR 0009) and resolves a process only to a pod
// UID, never to a namespace, so it cannot honor the filters security.md §10.2
// promises are applied on the node. It asks the controller, which holds them.
//
// Not a control channel (ADR 0001): the reply is pod identifiers from the
// admitted index, so it can only narrow what the node scans.
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
