// Package config loads the agent configuration from a YAML file. In-cluster
// that file is the Helm-rendered ConfigMap mounted into the pod; the agent
// reads it once at startup and is never reconfigured remotely (ADR 0001).
package config

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// The two roles have separate configuration schemas, and a setting belongs in
// the node's file only if the node can enforce it alone (ADR 0025). The symbol
// allow-list and the cost ceilings qualify: the node applies them to its own
// samples with no help from anyone, so no controller reply can widen them.
//
// They shared one schema until ADR 0025, and the cost was not hypothetical. The
// node's sample ConfigMap carried `eligibleNamespaces` under the comment
// "which workloads may be profiled at all (empty = none)", the node parsed it,
// logged it, and enforced nothing — a knob that read as deny-by-default and was
// inert. Separate types make that a parse error instead: `UnmarshalStrict`
// rejects a field the node's schema does not have, so a setting the node cannot
// honor stops the node instead of misleading its operator.
//
// That field is gone from both schemas now. Which workloads may be profiled is
// which workloads are collected, and Filters already says so; a second namespace
// list was the same intent expressed twice, with the opposite meaning for an
// empty value — and the shipped controller sample proved the trap by enabling
// profiling with an empty eligible set, which produces nothing, forever,
// silently.

// Config is the root of the controller's configuration file.
type Config struct {
	Filters    Filters             `json:"filters"`
	Spool      Spool               `json:"spool"`
	NodeIntake NodeIntake          `json:"nodeIntake"`
	Profiling  ControllerProfiling `json:"profiling"`
}

// NodeConfig is the root of the node role's configuration file. It holds only
// what the node enforces itself; everything else about profiling is the
// controller's (see the note above).
type NodeConfig struct {
	Profiling NodeProfiling `json:"profiling"`
}

// ControllerProfiling is the controller's half: whether to answer targeting
// queries at all, and how many workloads an answer may name.
//
// There is no separate eligible set. Which workloads may be profiled is which
// workloads are collected — Filters above — and a second namespace list would
// be the same intent expressed twice, in two shapes, with opposite meanings for
// an empty value (ADR 0025).
type ControllerProfiling struct {
	// Enabled turns on the controller's targeting endpoint. Off by default;
	// together with deploying the node DaemonSet it is the deliberate act that
	// starts profiling.
	Enabled bool `json:"enabled"`

	// TopN is how many top-consuming workloads an answer may name; 0 selects the
	// default. It is the only count bound in the profiling path: the node's own
	// cost is bounded by OverheadCeilingPercent and, ultimately, by its container
	// CPU limit.
	TopN int `json:"topN"`
}

// NodeProfiling is the node's half: the symbol allow-list that decides what may
// leave the node, and the ceilings on what profiling costs the node. Every field
// here is enforced from this file, on the node, against the node's own samples.
type NodeProfiling struct {
	// AllowedModulePrefixes is the symbol allow-list: Go module-path prefixes of
	// the customer's own code whose frames may leave the node (ADR 0011 §4).
	//
	// This is the load-bearing control of the whole profiling path. It is one
	// list per node, not per workload, and it is the only thing standing between
	// a compromised controller and the structure of the customer's code — which
	// is why it lives in a file Helm owns rather than arriving over the wire.
	AllowedModulePrefixes []string `json:"allowedModulePrefixes"`
	// ThirdPartySymbols is "drop" (default) or "keep": whether third-party
	// dependency frames are kept.
	ThirdPartySymbols string `json:"thirdPartySymbols"`

	// CaptureDurationSeconds is how long one capture runs; 0 selects 60s.
	CaptureDurationSeconds int `json:"captureDurationSeconds"`
	// IntervalSeconds is the gap between capture rounds; 0 selects the default.
	IntervalSeconds int `json:"intervalSeconds"`
	// OverheadCeilingPercent bounds the profiler's cost as a duty-cycle limit
	// (capture duration over the round interval, at the sampling rate); 0 selects
	// the default.
	OverheadCeilingPercent int `json:"overheadCeilingPercent"`
}

// Defaults and enumerations for profiling, applied by the Normalized methods.
const (
	DefaultProfilingTopN                   = 5
	DefaultProfilingCaptureDurationSeconds = 60
	DefaultProfilingIntervalSeconds        = 300
	DefaultProfilingOverheadCeilingPercent = 5

	ThirdPartySymbolsDrop = "drop"
	ThirdPartySymbolsKeep = "keep"
)

// Normalized returns a copy with empty numeric fields replaced by their
// defaults. It does not invent an eligible set: an empty allow-list stays empty
// (admit none).
func (p ControllerProfiling) Normalized() ControllerProfiling {
	if p.TopN <= 0 {
		p.TopN = DefaultProfilingTopN
	}
	return p
}

// Normalized returns a copy with empty numeric and policy fields replaced by
// their defaults, so consumers do not each re-implement the empty-means-default
// rule.
func (p NodeProfiling) Normalized() NodeProfiling {
	if p.ThirdPartySymbols == "" {
		p.ThirdPartySymbols = ThirdPartySymbolsDrop
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

// Spool configures the local payload sink (ADR 0003, ADR 0007). The directory
// is an emptyDir and the agent offers no durability option for it (ADR 0026):
// unacknowledged payloads are loss-harmless, and a reschedule loses at most the
// span the backend had not yet acknowledged. Dir is a path, so an operator who
// wants that span to survive can mount something durable there — that is their
// decision to make, not a setting the agent carries.
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

// Load reads the controller's configuration file at path. An empty path yields
// the zero configuration: collect everything, honoring only the opt-out
// annotations. Unknown fields are an error, so a typo cannot silently disable a
// filter.
func Load(path string) (Config, error) {
	return load[Config](path)
}

// LoadNode reads the node role's configuration file at path. An empty path
// yields the zero configuration, which the caller normalizes into defaults.
//
// Unknown fields are an error here for a second reason beyond typos: the node's
// schema is deliberately narrower than the controller's, so a controller-only
// setting placed in a node ConfigMap stops the node rather than being parsed and
// ignored. That is the intended failure — a setting the node cannot enforce must
// not look like one it does (ADR 0025).
func LoadNode(path string) (NodeConfig, error) {
	return load[NodeConfig](path)
}

func load[T any](path string) (T, error) {
	var cfg T
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
