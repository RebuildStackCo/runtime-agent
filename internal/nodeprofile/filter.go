package nodeprofile

import "strings"

// This is the security boundary of the profiler, not a data-reduction step: a
// stack trace is a map of the customer's code structure, and this filter is what
// makes shipping one defensible (ADR 0011 §4, security.md §8). Its posture is the
// opposite of the scanner's infra deny-list (nodescan.ModuleFilter, which keeps
// by default): here we KEEP only what we can positively classify as safe and
// redact the rest, so a new or misclassified frame errs toward not leaving.
//
// Policy (decided in the slice-4 plan):
//   - client frames (module path on the allow-list)      -> keep
//   - standard library / runtime / the workload's own
//     main package (no domain in the package path)       -> keep
//   - kernel frames                                       -> keep (N1: not the
//     customer's code, and they make a profile readable)
//   - third-party module frames (a domain, not allowed)  -> redact by default,
//     keep only if ThirdPartyKeep is configured
//   - unsymbolized / other native frames                 -> redact
//
// Redaction (R2): a redacted frame is replaced by a single neutral placeholder,
// and consecutive redacted frames collapse into one, so the flame-graph shape is
// preserved but no identity — not even which dependency, nor its depth — leaves
// the node. Only kept frames and aggregate drop counts ever leave.

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
// module-path prefixes; it arrives from the node ConfigMap in a later slice.
type SymbolFilter struct {
	allowed    []string
	thirdParty ThirdPartyPolicy
}

// NewSymbolFilter builds a filter. allowedModulePrefixes are the customer module
// paths to keep (e.g. "github.com/acme/app"); an empty list still keeps stdlib,
// main, and kernel frames, and redacts every third-party module.
func NewSymbolFilter(allowedModulePrefixes []string, thirdParty ThirdPartyPolicy) *SymbolFilter {
	return &SymbolFilter{allowed: allowedModulePrefixes, thirdParty: thirdParty}
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
		// stdlib, runtime, internal/*, the workload's own main package, or a
		// bare native/kernel symbol.
		//
		// This exemption is wider than it should be, and saying so is the point
		// of the note (ADR 0039). It is written as "no dot in the first path
		// segment", which is true of the standard library — those frames carry
		// no structure of the customer's code — and also true of `main.*` and
		// of any module declared without a domain, which carry a great deal of
		// it: handlers, jobs, business functions. Those reach a shipped profile
		// regardless of the allow-list, including when the allow-list is empty.
		// Narrowing this is a behaviour change and belongs to its own decision
		// record; until then docs/security.md §7.2 and §8 name the gap.
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
