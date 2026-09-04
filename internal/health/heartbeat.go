package health

import (
	"sync"
	"time"
)

// Heartbeat is one role's proof that its own loop is still turning: the loop
// stamps it, and liveness asks how old the stamp is.
//
// It is stamped at the start of a pass rather than at the end, so a pass that
// hangs half way through goes stale instead of never being counted (ADR 0069).
type Heartbeat struct {
	mu   sync.Mutex
	last time.Time
	// deadline is how old a stamp may be before the role is no longer alive.
	deadline time.Duration
}

// NewHeartbeat returns a heartbeat stamped now, so a role is alive from the
// moment it is built rather than from its first pass.
func NewHeartbeat(now time.Time, deadline time.Duration) *Heartbeat {
	return &Heartbeat{last: now, deadline: deadline}
}

// Beat records that the loop reached the top of a pass.
func (h *Heartbeat) Beat(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = now
}

// Alive reports whether the last stamp is within the deadline, and by how much
// it is not.
func (h *Heartbeat) Alive(now time.Time) (bool, time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	age := now.Sub(h.last)
	return age <= h.deadline, age
}
