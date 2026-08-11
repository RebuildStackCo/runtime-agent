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

func TestLoadsProfiling(t *testing.T) {
	path := write(t, `
profiling:
  enabled: true
  eligibleNamespaces: ["shop", "api"]
  allowedModulePrefixes: ["github.com/acme/app"]
  thirdPartySymbols: keep
  topN: 3
  captureDurationSeconds: 30
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Profiling
	if !p.Enabled || len(p.EligibleNamespaces) != 2 || p.AllowedModulePrefixes[0] != "github.com/acme/app" {
		t.Fatalf("profiling parsed wrong: %+v", p)
	}
	if p.ThirdPartySymbols != "keep" || p.TopN != 3 || p.CaptureDurationSeconds != 30 {
		t.Errorf("profiling fields wrong: %+v", p)
	}
}

func TestProfilingNormalizedDefaults(t *testing.T) {
	p := Profiling{}.Normalized()
	if p.ThirdPartySymbols != ThirdPartySymbolsDrop {
		t.Errorf("thirdPartySymbols default = %q, want drop", p.ThirdPartySymbols)
	}
	if p.TopN != DefaultProfilingTopN || p.CaptureDurationSeconds != DefaultProfilingCaptureDurationSeconds ||
		p.IntervalSeconds != DefaultProfilingIntervalSeconds || p.OverheadCeilingPercent != DefaultProfilingOverheadCeilingPercent {
		t.Errorf("numeric defaults not applied: %+v", p)
	}
	// Normalized must not invent an eligible set.
	if len(p.EligibleNamespaces) != 0 {
		t.Errorf("Normalized invented an eligible set: %+v", p.EligibleNamespaces)
	}
	// A set value is preserved.
	if got := (Profiling{TopN: 9}).Normalized(); got.TopN != 9 {
		t.Errorf("set TopN overwritten: %d", got.TopN)
	}
}
