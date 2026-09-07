package sink

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// maxRecoveryBytes caps one file the startup read will decode. Comfortably
// above a busy cluster's window and far below the spool's own budget, so a file
// this large is a corrupt one rather than a large one.
const maxRecoveryBytes = 64 << 20

// Recovery is what one startup read of the spool found (ADR 0072).
type Recovery struct {
	// Records is every still-open window record recovered, ready to seed an
	// accumulator.
	Records []*rollup.Record
	// Windows is how many window files they came from.
	Windows int
	// Skipped is one error per file that could not be used. Nothing is deleted
	// or rewritten on the strength of a failed read: a payload this process
	// cannot decode is still one the backend can ingest.
	Skipped []error
}

// RecoverOpenWindows returns the usage records this spool holds for windows
// still open at now — what a previous process of this agent accumulated and
// snapshotted before it stopped.
//
// It is the only read the agent ever makes of its own spool, and it is a pure
// one: it changes no file, and its input is this agent's own prior output
// rather than anything a backend said (ADR 0072). An empty or absent spool
// recovers nothing, which is the behaviour of every installation that has one.
func (s *Spool) RecoverOpenWindows(now time.Time) (Recovery, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return Recovery{}, fmt.Errorf("listing spool: %w", err)
	}
	var rec Recovery
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, usageSnapshotSuffix) {
			continue
		}
		k, ok := parseWindowName(strings.TrimSuffix(name, usageSnapshotSuffix))
		if !ok || !k.end().After(now) {
			continue // not ours to read, or a window that has already ended
		}
		// A window with a closed record was already made final. Reopening it
		// would let a later close supersede that record with a subset of it.
		if _, err := os.Stat(filepath.Join(s.dir, k.name()+".json")); err == nil {
			continue
		}
		records, err := s.readUsageSnapshot(name, k)
		if err != nil {
			rec.Skipped = append(rec.Skipped, err)
			continue
		}
		rec.Records = append(rec.Records, records...)
		rec.Windows++
	}
	s.countRecovery(len(rec.Records), rec.Windows, len(rec.Skipped))
	return rec, nil
}

// readUsageSnapshot decodes one snapshot file and refuses anything that is not
// the kind and window its name claims — a file another writer left here, one a
// crash truncated, or one whose records belong to a different window.
func (s *Spool) readUsageSnapshot(name string, k windowKey) ([]*rollup.Record, error) {
	path := filepath.Join(s.dir, name)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if info.Size() > maxRecoveryBytes {
		return nil, fmt.Errorf("refusing %s: %d bytes is past the %d-byte ceiling",
			name, info.Size(), maxRecoveryBytes)
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- a plain filename inside the agent's own spool
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	var payload usagePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", name, err)
	}
	if payload.Kind != kindUsageSnapshot {
		return nil, fmt.Errorf("refusing %s: it holds kind %q", name, payload.Kind)
	}
	if !payload.WindowStart.Equal(k.start) || payload.WindowSeconds != k.seconds {
		return nil, fmt.Errorf("refusing %s: it holds the window starting %s over %d s",
			name, payload.WindowStart, payload.WindowSeconds)
	}
	for _, r := range payload.Records {
		if r == nil || !r.WindowStart.Equal(k.start) || r.WindowSeconds != k.seconds {
			return nil, fmt.Errorf("refusing %s: a record in it belongs to another window", name)
		}
	}
	return payload.Records, nil
}

// parseWindowName is the inverse of windowKey.name.
func parseWindowName(name string) (windowKey, bool) {
	rest, ok := strings.CutPrefix(name, usageNamePrefix)
	if !ok {
		return windowKey{}, false
	}
	unix, seconds, ok := strings.Cut(rest, "-")
	if !ok {
		return windowKey{}, false
	}
	start, err := strconv.ParseInt(unix, 10, 64)
	if err != nil {
		return windowKey{}, false
	}
	length, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil || length <= 0 {
		return windowKey{}, false
	}
	return windowKey{start: time.Unix(start, 0).UTC(), seconds: length}, true
}
