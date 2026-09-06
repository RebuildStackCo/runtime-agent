package health

import (
	"testing"
	"time"
)

func TestAHeartbeatIsAliveFromTheMomentItIsBuilt(t *testing.T) {
	start := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	h := NewHeartbeat(start, time.Minute)
	if alive, age := h.Alive(start); !alive || age != 0 {
		t.Fatalf("Alive at construction = (%v, %v), want (true, 0): a role must not be dead before its first pass", alive, age)
	}
}

// The deadline is the only thing that decides, and the stamp is what moves it.
func TestAStampOlderThanTheDeadlineIsNotAlive(t *testing.T) {
	start := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	h := NewHeartbeat(start, 3*time.Minute)

	if alive, _ := h.Alive(start.Add(3 * time.Minute)); !alive {
		t.Error("a stamp exactly at the deadline is not alive; the boundary is inclusive")
	}
	if alive, age := h.Alive(start.Add(3*time.Minute + time.Second)); alive {
		t.Errorf("a stamp %v old is still alive against a 3m deadline", age)
	}
	h.Beat(start.Add(4 * time.Minute))
	if alive, _ := h.Alive(start.Add(4 * time.Minute)); !alive {
		t.Error("a beat did not revive the heartbeat")
	}
}

// TestTheStampAndTheDeadlineAreReadableForTheMetrics. An alert on the exposed
// stamp must fire on the same condition /livez answers no for, so both come
// from the heartbeat rather than from a second clock (ADR 0070 §4).
func TestTheStampAndTheDeadlineAreReadableForTheMetrics(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	h := NewHeartbeat(start, 90*time.Second)
	if got := h.Last(); !got.Equal(start) {
		t.Errorf("Last() = %v, want %v", got, start)
	}
	if got := h.Deadline(); got != 90*time.Second {
		t.Errorf("Deadline() = %v, want 90s", got)
	}

	next := start.Add(time.Minute)
	h.Beat(next)
	if got := h.Last(); !got.Equal(next) {
		t.Errorf("Last() = %v after a beat, want %v", got, next)
	}
	// The two agree by construction: the probe is not alive exactly when the
	// stamp is older than the deadline the metric reports.
	alive, _ := h.Alive(h.Last().Add(h.Deadline() + time.Second))
	if alive {
		t.Error("a stamp past the exposed deadline still reads as alive")
	}
}
