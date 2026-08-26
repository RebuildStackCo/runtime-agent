package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestParseRole(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantRole string
		wantRest []string
	}{
		{"no args defaults to controller", nil, roleController, nil},
		{"leading flags stay controller", []string{"-config", "/etc/agent.yaml"}, roleController, []string{"-config", "/etc/agent.yaml"}},
		{"explicit controller", []string{"controller", "-config", "x"}, roleController, []string{"-config", "x"}},
		{"node role", []string{"node", "-interval", "0"}, roleNode, []string{"-interval", "0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role, rest := parseRole(tc.args)
			if role != tc.wantRole {
				t.Errorf("role = %q, want %q", role, tc.wantRole)
			}
			if len(rest) != len(tc.wantRest) {
				t.Fatalf("rest = %v, want %v", rest, tc.wantRest)
			}
			for i := range rest {
				if rest[i] != tc.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], tc.wantRest[i])
				}
			}
		})
	}
}

// TestRunNodeSinglePass runs the node role in single-pass mode against an empty
// proc tree: it must complete without touching a Kubernetes client and return
// nil. It is the guard that the node role has no API dependency.
func TestRunNodeSinglePass(t *testing.T) {
	// The node refuses to start without a name to speak for (ADR 0040).
	t.Setenv("NODE_NAME", "node-1")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan error, 1)
	go func() {
		done <- runNode(context.Background(), logger, []string{"-proc", t.TempDir(), "-interval", "0"})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runNode single pass returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runNode single pass did not return")
	}
}
