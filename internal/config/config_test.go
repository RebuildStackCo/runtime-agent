package config

import (
	"os"
	"path/filepath"
	"strings"
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
  topN: 3
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Profiling
	if !p.Enabled || p.TopN != 3 {
		t.Fatalf("controller profiling parsed wrong: %+v", p)
	}
}

func TestLoadsNodeProfiling(t *testing.T) {
	path := write(t, `
profiling:
  allowedModulePrefixes: ["github.com/acme/app"]
  thirdPartySymbols: keep
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
	if p.ThirdPartySymbols != "keep" || p.CaptureDurationSeconds != 30 {
		t.Errorf("node profiling fields wrong: %+v", p)
	}
}

// Profiling scope is collection scope: there is no second namespace list, in
// either schema. The shipped controller sample once enabled profiling with an
// empty eligible set, which produced nothing forever and said nothing about it
// (ADR 0025).
func TestNeitherSchemaHasAnEligibleSet(t *testing.T) {
	for _, c := range []struct{ name, yaml string }{
		{"eligibleNamespaces", "profiling:\n  eligibleNamespaces: [\"shop\"]\n"},
		{"eligibleWorkloads", "profiling:\n  eligibleWorkloads: [\"web\"]\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Load(write(t, c.yaml)); err == nil {
				t.Errorf("the controller schema still accepts %s", c.name)
			}
			if _, err := LoadNode(write(t, c.yaml)); err == nil {
				t.Errorf("the node schema still accepts %s", c.name)
			}
		})
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
	// The allow-list and the third-party policy left this list when the
	// controller started filtering pulled profiles itself: it enforces them now,
	// so accepting them is the rule holding rather than bending (ADR 0058 §4).
	// What is left is the one setting only a node can act on.
	for _, c := range []struct{ name, yaml string }{
		{"overhead ceiling", "profiling:\n  overheadCeilingPercent: 9\n"},
		{"capture duration", "profiling:\n  captureDurationSeconds: 30\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Load(write(t, c.yaml)); err == nil {
				t.Errorf("Load accepted %s, which only the node enforces", c.name)
			}
		})
	}
}

// And the two that moved must actually arrive, or the controller would filter
// against an allow-list the operator wrote and it never read.
func TestControllerConfigCarriesTheSymbolAllowList(t *testing.T) {
	cfg, err := Load(write(t, "profiling:\n  allowedModulePrefixes: [\"github.com/acme/lib\"]\n  thirdPartySymbols: keep\n"))
	if err != nil {
		t.Fatalf("the controller schema rejects the allow-list it enforces: %v", err)
	}
	if got := cfg.Profiling.AllowedModulePrefixes; len(got) != 1 || got[0] != "github.com/acme/lib" {
		t.Errorf("allowedModulePrefixes = %v", got)
	}
	if cfg.Profiling.ThirdPartySymbols != "keep" {
		t.Errorf("thirdPartySymbols = %q", cfg.Profiling.ThirdPartySymbols)
	}
}

func TestControllerProfilingNormalizedDefaults(t *testing.T) {
	p := ControllerProfiling{}.Normalized()
	if p.TopN != DefaultProfilingTopN {
		t.Errorf("topN default = %d, want %d", p.TopN, DefaultProfilingTopN)
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
	if p.CaptureDurationSeconds != DefaultProfilingCaptureDurationSeconds ||
		p.IntervalSeconds != DefaultProfilingIntervalSeconds ||
		p.OverheadCeilingPercent != DefaultProfilingOverheadCeilingPercent {
		t.Errorf("numeric defaults not applied: %+v", p)
	}
	// Normalized must not invent an allow-list: an empty one keeps every frame
	// out, which is the safe direction.
	if len(p.AllowedModulePrefixes) != 0 {
		t.Errorf("Normalized invented a symbol allow-list: %+v", p.AllowedModulePrefixes)
	}
	if got := (NodeProfiling{CaptureDurationSeconds: 9}).Normalized(); got.CaptureDurationSeconds != 9 {
		t.Errorf("set CaptureDurationSeconds overwritten: %d", got.CaptureDurationSeconds)
	}
}

// A value outside the enumeration used to parse cleanly and then mean "drop",
// because nothing compared it to anything (ADR 0066). It governs which frames
// may leave the node, so it fails the start instead.
func TestAnUnknownThirdPartySymbolsValueIsAStartupFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		load func(string) error
	}{
		{"controller", "profiling:\n  thirdPartySymbols: Keep\n",
			func(p string) error { _, err := Load(p); return err }},
		{"node", "profiling:\n  thirdPartySymbols: yes\n",
			func(p string) error { _, err := LoadNode(p); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.load(write(t, tc.body))
			if err == nil {
				t.Fatal("an unrecognized policy was accepted; it would have silently meant drop")
			}
			if !strings.Contains(err.Error(), "thirdPartySymbols") {
				t.Errorf("error = %v, want it to name the field the operator has to fix", err)
			}
		})
	}
}

// The two policies and the empty value that selects the default all load. The
// empty case is the one a default install ships (charts/runtime-agent renders
// the key only when set), so a rule that rejected it would break every install.
func TestTheThirdPartySymbolsEnumerationLoads(t *testing.T) {
	for _, value := range []string{"", ThirdPartySymbolsDrop, ThirdPartySymbolsKeep} {
		body := "profiling:\n  thirdPartySymbols: " + `"` + value + `"` + "\n"
		if _, err := Load(write(t, body)); err != nil {
			t.Errorf("controller rejected %q: %v", value, err)
		}
		if _, err := LoadNode(write(t, body)); err != nil {
			t.Errorf("node rejected %q: %v", value, err)
		}
	}
}
