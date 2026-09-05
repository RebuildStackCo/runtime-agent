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
