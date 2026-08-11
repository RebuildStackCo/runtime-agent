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
	Filters    Filters    `json:"filters"`
	Spool      Spool      `json:"spool"`
	NodeIntake NodeIntake `json:"nodeIntake"`
	Profiling  Profiling  `json:"profiling"`
}

// Profiling configures the eBPF CPU profiler (ADR 0011). It is shared by both
// roles: the controller reads Enabled, the eligible set, and TopN to bound the
// targeting reply; the node reads the symbol allow-list, capture cadence, and
// overhead ceiling to run the profiler. On the node the -enable-ebpf flag is the
// master switch and this config is additive (which workloads, which symbols, how
// often); on the controller Enabled turns the targeting endpoint on.
type Profiling struct {
	// Enabled turns on the controller's targeting endpoint. Off by default.
	Enabled bool `json:"enabled"`

	// EligibleNamespaces bounds which namespaces may be profiled at all. Empty
	// admits none: profiling is an explicit allow-list, not a deny-list (the
	// posture of a security boundary, ADR 0011 §4). The controller may pick
	// targets only within this set, and the node re-checks it.
	EligibleNamespaces []string `json:"eligibleNamespaces"`
	// EligibleWorkloads optionally restricts to specific workload names within
	// the eligible namespaces. Empty admits every workload in those namespaces.
	EligibleWorkloads []string `json:"eligibleWorkloads"`

	// AllowedModulePrefixes is the symbol allow-list: Go module-path prefixes of
	// the customer's own code whose frames may leave the node (ADR 0011 §4).
	AllowedModulePrefixes []string `json:"allowedModulePrefixes"`
	// ThirdPartySymbols is "drop" (default) or "keep": whether third-party
	// dependency frames are kept.
	ThirdPartySymbols string `json:"thirdPartySymbols"`

	// TopN is how many top-consuming workloads the controller returns; 0 selects
	// the default.
	TopN int `json:"topN"`
	// CaptureDurationSeconds is how long one capture runs; 0 selects 60s.
	CaptureDurationSeconds int `json:"captureDurationSeconds"`
	// IntervalSeconds is the gap between capture rounds; 0 selects the default.
	IntervalSeconds int `json:"intervalSeconds"`
	// OverheadCeilingPercent bounds the profiler's cost as a duty-cycle limit
	// (capture duration over the round interval, at the sampling rate); 0 selects
	// the default.
	OverheadCeilingPercent int `json:"overheadCeilingPercent"`
}

// Defaults and enumerations for profiling, applied by Profiling.Normalized.
const (
	DefaultProfilingTopN                   = 5
	DefaultProfilingCaptureDurationSeconds = 60
	DefaultProfilingIntervalSeconds        = 300
	DefaultProfilingOverheadCeilingPercent = 5

	ThirdPartySymbolsDrop = "drop"
	ThirdPartySymbolsKeep = "keep"
)

// Normalized returns a copy with empty numeric and policy fields replaced by
// their defaults, so consumers do not each re-implement the empty-means-default
// rule. It does not invent an eligible set: an empty allow-list stays empty
// (admit none).
func (p Profiling) Normalized() Profiling {
	if p.ThirdPartySymbols == "" {
		p.ThirdPartySymbols = ThirdPartySymbolsDrop
	}
	if p.TopN <= 0 {
		p.TopN = DefaultProfilingTopN
	}
	if p.CaptureDurationSeconds <= 0 {
		p.CaptureDurationSeconds = DefaultProfilingCaptureDurationSeconds
	}
	if p.IntervalSeconds <= 0 {
		p.IntervalSeconds = DefaultProfilingIntervalSeconds
	}
	if p.OverheadCeilingPercent <= 0 {
		p.OverheadCeilingPercent = DefaultProfilingOverheadCeilingPercent
	}
	return p
}

// NodeIntake configures the controller's receiver for node-role reports
// (ADR 0010). It is off unless enabled, so a controller-only install (no node
// DaemonSet) opens no port. When enabled, the controller validates each node
// token locally against the cluster JWKS — no TokenReview, no new RBAC verb.
type NodeIntake struct {
	// Enabled turns the receiver on. Off by default.
	Enabled bool `json:"enabled"`
	// ListenAddress is where the receiver listens (net/http form, e.g.
	// ":8080"). Empty selects the built-in default when enabled.
	ListenAddress string `json:"listenAddress"`
	// Audience is the projected-token audience the receiver requires; it must
	// match the audience the node DaemonSet's serviceAccountToken projection
	// requests. Empty selects the built-in default.
	Audience string `json:"audience"`
	// ExpectedSubject pins the accepted token subject to the node role's
	// ServiceAccount, e.g.
	// "system:serviceaccount:runtime-agent:runtime-agent-node". Empty accepts
	// any subject that satisfies the audience — set it in production.
	ExpectedSubject string `json:"expectedSubject"`
}

// Defaults for the node-intake receiver, applied when enabled and the
// corresponding field is empty.
const (
	DefaultNodeIntakeListenAddress = ":8080"
	DefaultNodeIntakeAudience      = "rebuildstack-controller"
)

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
