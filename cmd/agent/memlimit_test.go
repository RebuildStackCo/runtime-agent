package main

import (
	"math"
	"testing"
)

// The fraction is the decision (ADR 0068 §5), so it is asserted as a number
// against the limit the chart ships by default: 256Mi leaves a 51 MiB reserve
// for the mapped text GOMEMLIMIT cannot see.
func TestTheHeapCeilingLeavesTheReserveTheChartsDefaultLimitNeeds(t *testing.T) {
	limit := int64(256 << 20)
	got, err := memoryLimit("268435456")
	if err != nil {
		t.Fatalf("memoryLimit: %v", err)
	}
	if want := int64(float64(limit) * 0.80); got != want {
		t.Errorf("memoryLimit(256Mi) = %d, want %d", got, want)
	}
	if reserve := limit - got; reserve < 50<<20 {
		t.Errorf("reserve is %d bytes; the binary maps 61.5 MiB of text, rodata and pclntab", reserve)
	}
}

// A value the agent cannot use must leave the Go default in place rather than
// produce a ceiling of its own. Zero is the case that matters: it is what a
// limit of "0" would render to, and a heap ceiling of zero is a process that
// collects continuously and does nothing else.
func TestAnUnusableValueYieldsNoLimit(t *testing.T) {
	for _, raw := range []string{"", "0", "-1", "256Mi", "1e9", " 12"} {
		if got, err := memoryLimit(raw); err == nil {
			t.Errorf("memoryLimit(%q) = %d, want an error", raw, got)
		}
	}
}

// The downward API emits the limit in bytes and a large node's allocatable
// memory overflows nothing, but the multiplication is float64: past 2^53 it
// stops being exact, so the ceiling must still be a limit and not a wrap.
func TestALargeLimitStaysBelowItself(t *testing.T) {
	got, err := memoryLimit("9007199254740992") // 8 PiB, past float64's exact range
	if err != nil {
		t.Fatalf("memoryLimit: %v", err)
	}
	if got <= 0 || got >= math.MaxInt64/2 {
		t.Errorf("memoryLimit(8Pi) = %d, want a positive ceiling below the limit", got)
	}
}
