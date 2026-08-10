package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// reportShipper delivers a node scan result to the controller over HTTP
// (ADR 0010). The node always initiates; authentication is a projected
// ServiceAccount token read fresh from tokenPath on every send, so the
// kubelet's rotation of that file needs no restart. Nothing is persisted on the
// node — the token lives only on the kubelet-managed tmpfs path (ADR 0008).
type reportShipper struct {
	endpoint  string
	tokenPath string
	node      string
	client    *http.Client
}

// newReportShipper builds a shipper for endpoint, or returns nil when endpoint
// is empty — the log-only mode that keeps the node role usable without a
// controller (and keeps the existing node e2e unchanged).
func newReportShipper(endpoint, tokenPath, node string) *reportShipper {
	if endpoint == "" {
		return nil
	}
	return &reportShipper{
		endpoint:  endpoint,
		tokenPath: tokenPath,
		node:      node,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ship POSTs one scan result. It re-reads the token per call (rotation), sets
// the bearer credential, and treats any non-2xx as an error. Delivery is
// best-effort by design: a failure is logged by the caller and the next scan
// pass retries — the controller reconstructs inventory from re-scans, so a lost
// report costs nothing (ADR 0010, loss-harmless).
func (s *reportShipper) ship(ctx context.Context, res nodescan.Result) error {
	token, err := s.readToken()
	if err != nil {
		return fmt.Errorf("reading controller token: %w", err)
	}

	report := nodescan.Report{
		Node:     s.node,
		Binaries: res.Binaries,
		Counters: res.Counters,
	}
	body, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encoding report: %w", err)
	}

	// The endpoint is the controller URL from the node's own configuration
	// (a flag set by the DaemonSet manifest / ConfigMap, invariant 1), not
	// caller-influenced input; the node has exactly one place to send.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body)) // #nosec G704 -- endpoint is operator-set config, not tainted input
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := s.client.Do(req) // #nosec G704 -- endpoint is operator-set config, not tainted input
	if err != nil {
		return fmt.Errorf("posting report: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain a bounded amount so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("controller rejected report: status %d", resp.StatusCode)
	}
	return nil
}

// readToken reads and trims the projected token file.
func (s *reportShipper) readToken() (string, error) {
	raw, err := os.ReadFile(s.tokenPath) // #nosec G304,G703 -- path is an operator-set flag, the projected-token mount
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", s.tokenPath)
	}
	return token, nil
}
