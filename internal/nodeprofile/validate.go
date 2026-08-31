package nodeprofile

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/pprof/profile"
)

// ErrInvalidProfile marks a profile that must not be shipped. The caller counts
// it and moves on — "not sent" beats "sent and wrong" (ADR 0011 §5).
var ErrInvalidProfile = errors.New("nodeprofile: invalid profile")

// Validate reports whether gzipped pprof bytes are safe and useful to ship: they
// parse, carry a cpu/nanoseconds sample type, and contain at least one service
// frame — not runtime.*, not the [filtered] placeholder. A profile in which no
// frame is the workload's own code describes no workload, and is rejected.
//
// Presence, not rank: a profile whose top is entirely the collector is what a GC
// problem looks like, and refusing it turned the strongest evidence of one into
// silence (ADR 0063). Errors wrap ErrInvalidProfile.
func Validate(gzpprof []byte) error {
	p, err := profile.ParseData(gzpprof)
	if err != nil {
		return fmt.Errorf("%w: parse: %w", ErrInvalidProfile, err)
	}
	if !hasCPUNanos(p) {
		return fmt.Errorf("%w: sample type is not cpu/nanoseconds", ErrInvalidProfile)
	}
	if !hasServiceFunction(p) {
		return fmt.Errorf("%w: no service frame anywhere (only runtime/filtered)", ErrInvalidProfile)
	}
	return nil
}

func hasCPUNanos(p *profile.Profile) bool {
	for _, st := range p.SampleType {
		if st.Type == "cpu" && st.Unit == "nanoseconds" {
			return true
		}
	}
	return false
}

// hasServiceFunction reports whether any sampled frame is the workload's own
// code. It walks the samples rather than the profile's function table, so the
// answer does not depend on the table holding only what the samples reference.
func hasServiceFunction(p *profile.Profile) bool {
	for _, s := range p.Sample {
		for _, loc := range s.Location {
			for _, ln := range loc.Line {
				if ln.Function != nil && isServiceFunc(ln.Function.Name) {
					return true
				}
			}
		}
	}
	return false
}

// isServiceFunc reports whether a function name represents the workload's own
// code rather than the Go runtime or a redacted frame.
func isServiceFunc(name string) bool {
	if name == "" || name == RedactedFrame {
		return false
	}
	if name == "runtime" || strings.HasPrefix(name, "runtime.") {
		return false
	}
	return true
}
