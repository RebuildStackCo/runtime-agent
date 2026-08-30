package nodeprofile

import "strings"

// The profiler's security boundary, not a data-reduction step: a stack trace is
// a map of the customer's code structure (ADR 0011 §4, ADR 0041, security.md §8).
//
// Keep only what can be positively classified — allow-listed modules, paths with
// no domain, kernel frames — and redact the rest, so a misclassified frame errs
// toward not leaving. A redacted frame becomes one neutral placeholder and
// consecutive ones collapse: the shape survives, no identity leaves.

// RedactedFrame is the neutral placeholder that replaces a redacted frame. It
// carries no identity.
const RedactedFrame = "[filtered]"

// ThirdPartyPolicy controls whether third-party dependency frames are kept.
type ThirdPartyPolicy string

const (
	// ThirdPartyDrop redacts third-party frames. It is the default (the zero
	// value "" is treated as drop).
	ThirdPartyDrop ThirdPartyPolicy = "drop"
	// ThirdPartyKeep keeps third-party frames.
	ThirdPartyKeep ThirdPartyPolicy = "keep"
)

// SymbolFilter reduces captured samples to allowed frames. It is stateless;
// counting is returned per call. The allow-list holds the customer's own Go
// module-path prefixes: those the node's ConfigMap names, and those the profiled
// build states it was compiled from (ADR 0059).
type SymbolFilter struct {
	allowed    []string
	thirdParty ThirdPartyPolicy
}

// NewSymbolFilter builds a filter. allowedModulePrefixes are the customer module
// paths to keep (e.g. "github.com/acme/app"); an empty list still keeps stdlib,
// main, and kernel frames, and redacts every third-party module.
func NewSymbolFilter(allowedModulePrefixes []string, thirdParty ThirdPartyPolicy) *SymbolFilter {
	// Entries that cannot be a module path are dropped rather than kept as a
	// prefix matching half the internet. Part of this list now comes from
	// binaries, so it has to survive one declaring something absurd (ADR 0059 §3).
	allowed := make([]string, 0, len(allowedModulePrefixes))
	for _, prefix := range allowedModulePrefixes {
		if ValidModulePrefix(prefix) {
			allowed = append(allowed, prefix)
		}
	}
	return &SymbolFilter{allowed: allowed, thirdParty: thirdParty}
}

// FilterCounters are aggregate-only drop counts. They are safe to log and ship;
// they never carry the identity of a redacted frame (CLAUDE.md invariant 6).
type FilterCounters struct {
	ThirdPartyDropped   uint64 // third-party frames redacted
	UnsymbolizedDropped uint64 // unsymbolized / other native frames redacted
	SamplesFiltered     uint64 // samples that had at least one frame redacted
}

// Filter returns a copy of samples with disallowed frames redacted, plus the
// aggregate counts. Input samples are not mutated.
func (f *SymbolFilter) Filter(samples []Sample) ([]Sample, FilterCounters) {
	var c FilterCounters
	out := make([]Sample, 0, len(samples))
	for _, s := range samples {
		fs, redacted := f.filterFrames(s.Frames, &c)
		if redacted {
			c.SamplesFiltered++
		}
		s.Frames = fs
		out = append(out, s)
	}
	return out, c
}

// filterFrames applies the policy to one stack, collapsing consecutive redacted
// frames into a single placeholder (R2). It reports whether anything was
// redacted.
func (f *SymbolFilter) filterFrames(frames []Frame, c *FilterCounters) ([]Frame, bool) {
	out := make([]Frame, 0, len(frames))
	redactedAny := false
	prevRedacted := false
	for _, fr := range frames {
		if f.keep(fr, c) {
			out = append(out, fr)
			prevRedacted = false
			continue
		}
		redactedAny = true
		if prevRedacted {
			continue // collapse consecutive redacted frames
		}
		out = append(out, Frame{Function: RedactedFrame, Kind: "filtered"})
		prevRedacted = true
	}
	return out, redactedAny
}

// keep classifies one frame and, when it redacts, increments the matching
// counter. It never records the frame's identity.
func (f *SymbolFilter) keep(fr Frame, c *FilterCounters) bool {
	if fr.Kind == "kernel" {
		return true // N1: kernel frames are not the customer's code
	}
	if fr.Function == "" {
		c.UnsymbolizedDropped++
		return false
	}
	pkg := packagePath(fr.Function)
	if !hasDomain(pkg) {
		// Wider than the standard library, deliberately: a public module path
		// must begin with a domain, so "no domain" means stdlib or code the
		// customer did not publish. Gating those would rebuild the second list
		// ADR 0025 abolished (ADR 0041).
		return true
	}
	if f.isAllowed(pkg) {
		return true
	}
	// A domain-bearing package that is not the customer's: third-party.
	if f.thirdParty == ThirdPartyKeep {
		return true
	}
	c.ThirdPartyDropped++
	return false
}

func (f *SymbolFilter) isAllowed(pkg string) bool {
	for _, prefix := range f.allowed {
		if pkg == prefix || strings.HasPrefix(pkg, prefix+"/") {
			return true
		}
	}
	return false
}

// packagePath extracts the Go package import path from a symbolized function
// name such as "github.com/acme/app/svc.(*S).Do" -> "github.com/acme/app/svc",
// "runtime.mallocgc" -> "runtime", "main.process" -> "main". A name with neither
// "/" nor "." (a bare native/kernel symbol) is returned unchanged.
func packagePath(fn string) string {
	slash := strings.LastIndex(fn, "/")
	// The package name is the segment after the last "/", up to its first ".".
	tail := fn[slash+1:]
	if dot := strings.IndexByte(tail, '.'); dot >= 0 {
		tail = tail[:dot]
	}
	if slash < 0 {
		return tail
	}
	return fn[:slash+1] + tail
}

// hasDomain reports whether a package path's first segment looks like a domain
// (contains a "."), which is how Go distinguishes an external module path
// ("github.com/...") from a standard-library path ("net/http", "runtime").
func hasDomain(pkg string) bool {
	first := pkg
	if i := strings.IndexByte(pkg, '/'); i >= 0 {
		first = pkg[:i]
	}
	return strings.Contains(first, ".")
}
