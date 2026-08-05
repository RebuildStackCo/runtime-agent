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
