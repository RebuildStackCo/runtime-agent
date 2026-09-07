package sink

import (
	"bytes"
	"crypto/sha256"
	"sync"
	"time"
)

// When a kind is written, as a property of the kind (ADR 0073).
//
// The cadence is a constant of this agent's protocol version. Nothing the
// backend sends can change it — there is no channel that could (ADR 0001) — so
// the backend reads the numbers from the published table for the agent version
// it already records, rather than being told them on the wire.

// Trigger is what decides that a payload of a kind is written.
type Trigger string

const (
	// TriggerAlways is written on every pass of the ticker that writes it,
	// whether or not anything changed. Floor and Ceiling are that ticker's
	// interval, and cmd/agent binds the two so neither can move alone.
	TriggerAlways Trigger = "always"
	// TriggerChanged is written on the first pass at or after Floor on which
	// the payload differs from the one last written under its key, and
	// unconditionally once Ceiling has passed.
	TriggerChanged Trigger = "changed"
	// TriggerEvent is not periodic at all: the payload is written when the
	// thing it records happens — a kill, a window closing, a build first seen.
	// Floor and Ceiling are zero.
	TriggerEvent Trigger = "event"
)

// Cadence is the gap between two writes of one natural key: never shorter than
// Floor, never longer than Ceiling, and decided between them by Trigger.
//
// The Ceiling is the load-bearing half. A superseding kind carries no ordering
// field (ADR 0027), so a change the predicate missed — a bug in it, a payload
// whose shape moved — would otherwise stand as the answer forever with nothing
// able to notice. The Ceiling turns "forever" into "at most one Ceiling".
type Cadence struct {
	Floor   time.Duration
	Ceiling time.Duration
	Trigger Trigger
}

// gate is the write side of a change-triggered cadence: the instant and the
// fingerprint of the last payload written under each natural key, which is the
// spool filename (ADR 0003). Held in memory only — losing it on a restart makes
// the next pass write, which is the safe direction.
type gate struct {
	mu   sync.Mutex
	last map[string]written
}

type written struct {
	at time.Time
	fp [sha256.Size]byte
}

// due answers whether a payload with these bytes is written now, and returns
// the fingerprint to record if the write lands. A key not written before always
// is: a fresh agent states the cluster it found before it starts comparing.
func (g *gate) due(key string, c Cadence, data []byte, now time.Time) ([sha256.Size]byte, bool) {
	if c.Trigger != TriggerChanged {
		return [sha256.Size]byte{}, true
	}
	fp := fingerprint(data)

	g.mu.Lock()
	defer g.mu.Unlock()
	prev, seen := g.last[key]
	switch {
	case !seen:
		return fp, true
	case now.Sub(prev.at) >= c.Ceiling:
		return fp, true
	case now.Sub(prev.at) < c.Floor:
		return fp, false
	default:
		return fp, fp != prev.fp
	}
}

// wrote records a landed write. Only change-triggered kinds are recorded: the
// others never consult the gate, and an entry for every profile and every OOM
// kill would be a map that only grows.
func (g *gate) wrote(key string, c Cadence, fp [sha256.Size]byte, now time.Time) {
	if c.Trigger != TriggerChanged {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = map[string]written{}
	}
	g.last[key] = written{at: now, fp: fp}
}

// prune forgets keys not written since cutoff. It rides the spool's sweep, and
// the cutoff is the same one: the gate remembers what the spool holds, and a
// key the spool has dropped will be written whole again anyway.
func (g *gate) prune(cutoff time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for key, w := range g.last {
		if w.at.Before(cutoff) {
			delete(g.last, key)
		}
	}
}

// capturedAtField is the encoder's rendering of a top-level `captured_at`:
// writePayload indents by two spaces per level, so this prefix matches at depth
// one and nowhere deeper.
var capturedAtField = []byte(`  "captured_at": `)

// fingerprint is what the predicate compares: the payload's own bytes with its
// capture instant elided. `captured_at` dates the write and not the state, so it
// differs on every pass by construction, and comparing it would make every
// predicate vacuous. Nothing else is elided — a projection is a set of fields
// whose changes go unseen for a Ceiling, and one that must be maintained by hand
// as a payload grows (ADR 0073 §3).
func fingerprint(data []byte) [sha256.Size]byte {
	h := sha256.New()
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i+1], data[i+1:]
		} else {
			data = nil
		}
		if bytes.HasPrefix(line, capturedAtField) {
			continue
		}
		h.Write(line)
	}
	var out [sha256.Size]byte
	h.Sum(out[:0])
	return out
}
