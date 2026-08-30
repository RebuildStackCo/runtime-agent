package nodeprofile

import (
	"maps"
	"slices"
	"strings"
	"sync/atomic"
)

// The node's answer to "whose code is this frame" (ADR 0059).
//
// The scanner already reads every kept binary's own module paths on this node.
// The profiler runs beside it in the same process and, until this existed,
// filtered every container against one configured list — so a cluster running
// five services with one prefix configured redacted four of them.

// ModuleIndex maps a container to the module paths its build compiles from
// source. It is written by the scanner's pass and read by the profiler's window,
// which are different goroutines, so the published map is replaced wholesale
// rather than mutated — the discipline targeting.Publisher uses on the
// controller for the same reason.
type ModuleIndex struct {
	current atomic.Pointer[map[string][]string]
}

// Publish replaces the index with one pass's findings. A container absent from
// the new map is a container no live process on this node belongs to, so its
// entry goes with it.
func (i *ModuleIndex) Publish(byContainer map[string][]string) {
	next := make(map[string][]string, len(byContainer))
	for cid, mods := range byContainer {
		next[cid] = slices.Clone(mods)
	}
	i.current.Store(&next)
}

// Modules returns the module paths of a container's build, or nothing when the
// scanner has not reported it yet — the first window after start, before the
// first pass completes.
func (i *ModuleIndex) Modules(containerID string) []string {
	m := i.current.Load()
	if m == nil {
		return nil
	}
	return (*m)[containerID]
}

// Size is how many containers the index covers, for the node's own log line.
func (i *ModuleIndex) Size() int {
	m := i.current.Load()
	if m == nil {
		return 0
	}
	return len(*m)
}

// Snapshot returns a copy of the index, for a caller that needs all of it.
func (i *ModuleIndex) Snapshot() map[string][]string {
	m := i.current.Load()
	if m == nil {
		return nil
	}
	return maps.Clone(*m)
}

// ValidModulePrefix reports whether a string is plausible as an allow-list
// entry: a domain-bearing first segment and at least one segment after it.
//
// It exists because part of the allow-list now comes from binaries rather than
// from a file. A build declaring its main module as a bare "github.com" would
// otherwise admit every module hosted there. The same check applies to a prefix
// an operator wrote, where the same mistake is available (ADR 0059 §3).
func ValidModulePrefix(prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	first, rest, ok := strings.Cut(prefix, "/")
	if !ok || rest == "" {
		return false
	}
	// A path with no domain is standard library or unpublished code, which the
	// filter keeps without an allow-list entry; one as an entry is meaningless
	// rather than dangerous, and is refused for being so.
	return strings.Contains(first, ".")
}
