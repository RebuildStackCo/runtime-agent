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

	"github.com/RebuildStackCo/runtime-agent/internal/nodeintake"
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
	return readProjectedToken(s.tokenPath)
}

// readProjectedToken reads and trims the projected ServiceAccount token file
// shared by both node→controller shippers (ADR 0008/0010). It is read fresh per
// send so the kubelet's rotation needs no restart.
func readProjectedToken(path string) (string, error) {
	raw, err := os.ReadFile(path) // #nosec G304,G703 -- path is an operator-set flag, the projected-token mount
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return token, nil
}

// profileShipper delivers one captured eBPF CPU profile to the controller's
// profile endpoint (ADR 0011 §5.5), over the same node-initiated, projected-
// token transport as the inventory shipper. Only already allow-list-filtered
// pprof bytes are ever sent (ADR 0011 §4); the node attaches only what it can see
// locally (pod UID / container ID), and the controller does the workload join.
type profileShipper struct {
	endpoint  string
	tokenPath string
	client    *http.Client
}

// newProfileShipper builds a shipper for endpoint, or returns nil when endpoint
// is empty (the log-only / capture-only mode).
func newProfileShipper(endpoint, tokenPath string) *profileShipper {
	if endpoint == "" {
		return nil
	}
	return &profileShipper{
		endpoint:  endpoint,
		tokenPath: tokenPath,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

// ship POSTs one profile report. Delivery is best-effort: a lost profile is
// simply not analyzed (loss-harmless, ADR 0003/0007).
func (s *profileShipper) ship(ctx context.Context, report nodeintake.ProfileReport) error {
	token, err := readProjectedToken(s.tokenPath)
	if err != nil {
		return fmt.Errorf("reading controller token: %w", err)
	}
	body, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encoding profile: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body)) // #nosec G704 -- endpoint is operator-set config, not tainted input
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := s.client.Do(req) // #nosec G704 -- endpoint is operator-set config, not tainted input
	if err != nil {
		return fmt.Errorf("posting profile: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("controller rejected profile: status %d", resp.StatusCode)
	}
	return nil
}
