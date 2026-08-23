package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestLoadsFilters(t *testing.T) {
	path := write(t, `
filters:
  namespaces:
    allow:
      - shop
      - team-*
    deny:
      - team-sandbox
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ns := cfg.Filters.Namespaces
	if len(ns.Allow) != 2 || ns.Allow[0] != "shop" || ns.Allow[1] != "team-*" {
		t.Errorf("allow = %v, want [shop team-*]", ns.Allow)
	}
	if len(ns.Deny) != 1 || ns.Deny[0] != "team-sandbox" {
		t.Errorf("deny = %v, want [team-sandbox]", ns.Deny)
	}
}

func TestEmptyPathIsZeroConfig(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ns := cfg.Filters.Namespaces
	if len(ns.Allow) != 0 || len(ns.Deny) != 0 {
		t.Errorf("zero config has filters: %+v", ns)
	}
}

func TestRejectsUnknownFields(t *testing.T) {
	// A typo must be an error, not a silently disabled filter.
	path := write(t, `
filters:
  namespases:
    deny: [shop]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a config with an unknown field")
	}
}

func TestMissingFileIsAnError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load accepted a missing file")
	}
}

func TestLoadsControllerProfiling(t *testing.T) {
	path := write(t, `
profiling:
  enabled: true
  eligibleNamespaces: ["shop", "api"]
  eligibleWorkloads: ["web"]
  topN: 3
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Profiling
	if !p.Enabled || len(p.EligibleNamespaces) != 2 || len(p.EligibleWorkloads) != 1 || p.TopN != 3 {
		t.Fatalf("controller profiling parsed wrong: %+v", p)
	}
}

func TestLoadsNodeProfiling(t *testing.T) {
	path := write(t, `
profiling:
  allowedModulePrefixes: ["github.com/acme/app"]
  thirdPartySymbols: keep
  maxTargetsPerWindow: 3
  captureDurationSeconds: 30
`)
	cfg, err := LoadNode(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Profiling
	if len(p.AllowedModulePrefixes) != 1 || p.AllowedModulePrefixes[0] != "github.com/acme/app" {
		t.Fatalf("node profiling parsed wrong: %+v", p)
	}
	if p.ThirdPartySymbols != "keep" || p.MaxTargetsPerWindow != 3 || p.CaptureDurationSeconds != 30 {
		t.Errorf("node profiling fields wrong: %+v", p)
	}
}

// The schemas are separate so that a setting the node cannot enforce stops it
// rather than being parsed and ignored. Before ADR 0025 both roles shared one
// type: the node's sample ConfigMap carried `eligibleNamespaces` under a comment
// promising deny-by-default, and the node read it, logged it, and enforced
// nothing.
func TestNodeConfigRejectsControllerSettings(t *testing.T) {
	for _, c := range []struct{ name, yaml string }{
		{"eligible namespaces", "profiling:\n  eligibleNamespaces: [\"shop\"]\n"},
		{"eligible workloads", "profiling:\n  eligibleWorkloads: [\"web\"]\n"},
		{"enabled", "profiling:\n  enabled: true\n"},
		{"controller topN", "profiling:\n  topN: 3\n"},
		{"collection filters", "filters:\n  namespaces:\n    allow: [\"shop\"]\n"},
		{"spool", "spool:\n  dir: /var/spool\n"},
		{"node intake", "nodeIntake:\n  enabled: true\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := LoadNode(write(t, c.yaml)); err == nil {
				t.Errorf("LoadNode accepted %s; a setting the node cannot enforce must not "+
					"look like one it does", c.name)
			}
		})
	}
}

// And the controller's schema does not silently accept the node's, for the same
// reason in reverse: a symbol allow-list set on the controller enforces nothing.
func TestControllerConfigRejectsNodeSettings(t *testing.T) {
	for _, c := range []struct{ name, yaml string }{
		{"symbol allow-list", "profiling:\n  allowedModulePrefixes: [\"github.com/acme/\"]\n"},
		{"third-party symbols", "profiling:\n  thirdPartySymbols: keep\n"},
		{"max targets per window", "profiling:\n  maxTargetsPerWindow: 3\n"},
		{"overhead ceiling", "profiling:\n  overheadCeilingPercent: 9\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Load(write(t, c.yaml)); err == nil {
				t.Errorf("Load accepted %s, which only the node enforces", c.name)
			}
		})
	}
}

func TestControllerProfilingNormalizedDefaults(t *testing.T) {
	p := ControllerProfiling{}.Normalized()
	if p.TopN != DefaultProfilingTopN {
		t.Errorf("topN default = %d, want %d", p.TopN, DefaultProfilingTopN)
	}
	// Normalized must not invent an eligible set.
	if len(p.EligibleNamespaces) != 0 {
		t.Errorf("Normalized invented an eligible set: %+v", p.EligibleNamespaces)
	}
	if got := (ControllerProfiling{TopN: 9}).Normalized(); got.TopN != 9 {
		t.Errorf("set TopN overwritten: %d", got.TopN)
	}
}

func TestNodeProfilingNormalizedDefaults(t *testing.T) {
	p := NodeProfiling{}.Normalized()
	if p.ThirdPartySymbols != ThirdPartySymbolsDrop {
		t.Errorf("thirdPartySymbols default = %q, want drop", p.ThirdPartySymbols)
	}
	if p.MaxTargetsPerWindow != DefaultProfilingMaxTargetsPerWindow ||
		p.CaptureDurationSeconds != DefaultProfilingCaptureDurationSeconds ||
		p.IntervalSeconds != DefaultProfilingIntervalSeconds ||
		p.OverheadCeilingPercent != DefaultProfilingOverheadCeilingPercent {
		t.Errorf("numeric defaults not applied: %+v", p)
	}
	// Normalized must not invent an allow-list: an empty one keeps every frame
	// out, which is the safe direction.
	if len(p.AllowedModulePrefixes) != 0 {
		t.Errorf("Normalized invented a symbol allow-list: %+v", p.AllowedModulePrefixes)
	}
	if got := (NodeProfiling{MaxTargetsPerWindow: 9}).Normalized(); got.MaxTargetsPerWindow != 9 {
		t.Errorf("set MaxTargetsPerWindow overwritten: %d", got.MaxTargetsPerWindow)
	}
}
