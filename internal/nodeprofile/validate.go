package nodeprofile

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/pprof/profile"
)

// ErrInvalidProfile marks a profile that must not be shipped. The caller counts
// it and moves on — "not sent" beats "sent and wrong" (ADR 0011 §5).
var ErrInvalidProfile = errors.New("nodeprofile: invalid profile")

// topServiceFunctions bounds how many of the highest-value functions we require
// a service function to appear among.
const topServiceFunctions = 5

// Validate reports whether gzipped pprof bytes are safe and useful to ship: they
// parse, carry a cpu/nanoseconds sample type, and have a service function — not
// runtime.*, not the [filtered] placeholder — among the top functions by
// cumulative value. A profile that is only runtime/filtered activity is not worth
// shipping and is rejected. Errors wrap ErrInvalidProfile.
func Validate(gzpprof []byte) error {
	p, err := profile.ParseData(gzpprof)
	if err != nil {
		return fmt.Errorf("%w: parse: %w", ErrInvalidProfile, err)
	}
	if !hasCPUNanos(p) {
		return fmt.Errorf("%w: sample type is not cpu/nanoseconds", ErrInvalidProfile)
	}
	if !hasTopServiceFunction(p) {
		return fmt.Errorf("%w: no service function among the top (only runtime/filtered)", ErrInvalidProfile)
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

// hasTopServiceFunction ranks functions by cumulative sample value and checks
// whether a service function is among the top few.
func hasTopServiceFunction(p *profile.Profile) bool {
	val := map[string]int64{}
	for _, s := range p.Sample {
		var v int64
		if len(s.Value) > 0 {
			v = s.Value[0]
		}
		seen := map[string]bool{}
		for _, loc := range s.Location {
			for _, ln := range loc.Line {
				name := ln.Function.Name
				if !seen[name] {
					val[name] += v
					seen[name] = true
				}
			}
		}
	}

	type fv struct {
		name string
		v    int64
	}
	ranked := make([]fv, 0, len(val))
	for n, v := range val {
		ranked = append(ranked, fv{n, v})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].v != ranked[j].v {
			return ranked[i].v > ranked[j].v
		}
		return ranked[i].name < ranked[j].name
	})
	for i, f := range ranked {
		if i >= topServiceFunctions {
			break
		}
		if isServiceFunc(f.name) {
			return true
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
