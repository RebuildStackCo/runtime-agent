package config

import (
	"os"
	"time"
)

// Shape is what the coverage payload says about the configuration in force:
// how many entries each filter list holds and which switches are on, and since
// when. It carries no name from the file (ADR 0054 §2).
//
// Deliberately not a hash: a digest of a handful of short namespace names is
// reversible by trying the plausible ones, and what it would expose is the deny
// list — the set the customer chose to hide (ADR 0054 §2).
type Shape struct {
	// Since is when the configuration in force took effect: the file's
	// modification time, or the process start when there is no file. It answers
	// "have the filters changed since the last upload" with a time rather than
	// with an opaque token, and a time is not content.
	Since time.Time `json:"since"`
	// NamespacesAllowed and NamespacesDenied are entry counts. An empty allow
	// list admits every namespace, so zero and three are different postures
	// rather than more or less of one.
	NamespacesAllowed int `json:"namespaces_allowed"`
	NamespacesDenied  int `json:"namespaces_denied"`
	// ProfilingEnabled is the controller's half of the profiling switch; TopN is
	// how many workloads an answer may name.
	ProfilingEnabled bool `json:"profiling_enabled"`
	ProfilingTopN    int  `json:"profiling_top_n,omitempty"`
	// NodeIntakeEnabled is whether the receiver for node reports is open.
	NodeIntakeEnabled bool `json:"node_intake_enabled"`
	// SpoolMaxAgeHours is 0 when the agent's own default applies.
	SpoolMaxAgeHours int `json:"spool_max_age_hours,omitempty"`
}

// Describe reduces a loaded configuration to its shape. path is the file it came
// from; started is the fallback for Since when there is no file to date.
func (c Config) Describe(path string, started time.Time) Shape {
	return Shape{
		Since:             configSince(path, started),
		NamespacesAllowed: len(c.Filters.Namespaces.Allow),
		NamespacesDenied:  len(c.Filters.Namespaces.Deny),
		ProfilingEnabled:  c.Profiling.Enabled,
		ProfilingTopN:     c.Profiling.TopN,
		NodeIntakeEnabled: c.NodeIntake.Enabled,
		SpoolMaxAgeHours:  c.Spool.MaxAgeHours,
	}
}

// configSince is the file's modification time, falling back to started.
//
// The fallback is not a degradation in practice: the chart stamps a checksum of
// the ConfigMap onto the controller Deployment, so editing the configuration
// replaces the pod. The file cannot change under a running agent, which is why
// this needs no watch.
func configSince(path string, started time.Time) time.Time {
	if path == "" {
		return started.UTC()
	}
	info, err := os.Stat(path)
	if err != nil {
		return started.UTC()
	}
	return info.ModTime().UTC()
}
