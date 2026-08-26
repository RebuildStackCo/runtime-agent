package nodeintake

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeauth"
)

// profileMaxBodyBytes caps a single profile POST. A gzipped, allow-list-filtered
// pprof for one workload over one capture window is small (tens of KB to a few
// MB); this is generous headroom whose purpose is to refuse a runaway or hostile
// body outright.
const profileMaxBodyBytes = 16 << 20 // 16 MiB

// ProfileReport is one captured eBPF CPU profile shipped from a node to the
// controller (ADR 0011 §6). The node knows only what it can see locally: the
// process identity (pod UID and container ID parsed from the cgroup, ADR 0009),
// the capture window, and the already-symbolized, allow-list-filtered pprof
// bytes (ADR 0011 §4). The controller joins the pod UID / container ID to a
// namespace, workload, and image digest via PodWatcher — the node never learns
// those (it holds no Kubernetes identity, ADR 0009). Pprof is gzipped protobuf,
// base64 in JSON.
type ProfileReport struct {
	Node         string    `json:"node"`
	PodUID       string    `json:"pod_uid"`
	ContainerID  string    `json:"container_id"`
	PID          int32     `json:"pid"`
	CaptureStart time.Time `json:"capture_start"`
	CaptureEnd   time.Time `json:"capture_end"`
	Pprof        []byte    `json:"pprof"`
}

// ProfileHandler authenticates and decodes a node profile report, then invokes
// onProfile with the verified caller identity and the report. It mirrors Handler
// (same one-way discipline: the response is an acknowledgment or an error, never
// anything the node acts on — ADR 0001/0010).
type ProfileHandler struct {
	verifier     TokenVerifier
	onProfile    func(nodeauth.Identity, ProfileReport)
	logger       *slog.Logger
	maxBodyBytes int64
}

// NewProfileHandler builds a profile receiver handler. onProfile may be nil (the
// report is authenticated and decoded, then dropped).
func NewProfileHandler(verifier TokenVerifier, logger *slog.Logger, onProfile func(nodeauth.Identity, ProfileReport)) *ProfileHandler {
	return &ProfileHandler{
		verifier:     verifier,
		onProfile:    onProfile,
		logger:       logger,
		maxBodyBytes: profileMaxBodyBytes,
	}
}

// ServeHTTP accepts POST with a bearer token, verifies it, decodes a
// ProfileReport, and acknowledges. Failure paths mirror Handler: a coarse 401
// that does not distinguish malformed from well-formed-but-untrusted.
func (h *ProfileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		h.logger.Warn("node profile rejected: token verification failed", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var report ProfileReport
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "profile too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "malformed profile", http.StatusBadRequest)
		return
	}
	if dec.More() {
		http.Error(w, "malformed profile", http.StatusBadRequest)
		return
	}
	if !authorizeNode(w, h.logger, "node profile", identity, report.Node) {
		return
	}

	if h.onProfile != nil {
		h.onProfile(identity, report)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(struct {
		Status string `json:"status"`
	}{Status: "accepted"})
}
