// Package config loads the agent configuration from a YAML file. In-cluster
// that file is the Helm-rendered ConfigMap mounted into the pod; the agent
// reads it once at startup and is never reconfigured remotely (ADR 0001).
package config

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Config is the root of the agent configuration file.
type Config struct {
	Filters Filters `json:"filters"`
	Spool   Spool   `json:"spool"`
}

// Spool configures the local payload sink (ADR 0003, ADR 0007). The
// directory's durability is the installation's choice: emptyDir by default,
// a PersistentVolume for those who want unacknowledged data to survive
// rescheduling.
type Spool struct {
	// Dir is where payload batches are written. Empty disables the local
	// sink — a log-only development mode.
	Dir string `json:"dir"`
	// MaxAgeHours caps how long unacknowledged payloads are kept; 0 means
	// the built-in default (24h).
	MaxAgeHours int `json:"maxAgeHours"`
}

// Filters controls what the agent collects. Everything not excluded here can
// still opt out per namespace or pod with the collect annotation.
type Filters struct {
	Namespaces NamespaceFilters `json:"namespaces"`
}

// NamespaceFilters selects namespaces by name; entries may use "*" as a
// wildcard. An empty Allow list admits every namespace; a non-empty one
// admits only matches. Deny always applies on top and wins on conflict.
type NamespaceFilters struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// Load reads the configuration file at path. An empty path yields the zero
// configuration: collect everything, honoring only the opt-out annotations.
// Unknown fields are an error, so a typo cannot silently disable a filter.
func Load(path string) (Config, error) {
	var cfg Config
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the path comes from the operator's own -config flag; reading it is the point
	if err != nil {
		return cfg, fmt.Errorf("reading config: %w", err)
	}
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return cfg, nil
}
