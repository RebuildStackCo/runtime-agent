package sink

import "sort"

// Why a payload left the spool. The age cutoff and the orphaned temp file are
// unconditional; bytes and files are the ceilings acting, and those two rising
// mean the backend is missing payloads that were written (ADR 0042).
const (
	EvictedAge        = "age"
	EvictedOrphanTemp = "orphan_temp"
	EvictedBytes      = "bytes"
	EvictedFiles      = "files"
)

// EvictionReasons is the closed set, for whoever renders it.
var EvictionReasons = []string{EvictedAge, EvictedOrphanTemp, EvictedBytes, EvictedFiles}

// Counters is what the spool did, cumulative since the process started, plus
// what the last sweep measured. Nothing here reaches a payload today: it is the
// agent's own operational state, and the shape of a spool block in
// `collection_coverage` is a protocol decision of its own (ADR 0070 §5).
type Counters struct {
	// Written and WriteFailures are per payload kind, and hold a zero entry for
	// every kind in the registry — "no usage_window has ever been written" is
	// the reading that matters, and it needs the series to exist.
	Written       map[string]int64
	WriteFailures map[string]int64
	// Evicted is per reason, with a zero entry for each.
	Evicted map[string]int64
	// Bytes and Files are what the last sweep counted after evicting, against
	// DefaultMaxBytes and DefaultMaxFiles. Zero before the first sweep runs.
	Bytes int64
	Files int64
}

// countWrite records one payload written, or one write that failed.
func (s *Spool) countWrite(kind string, failed bool) {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	if failed {
		s.writeFailures[kind]++
		return
	}
	s.written[kind]++
}

func (s *Spool) countEviction(reason string) {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	s.evicted[reason]++
}

func (s *Spool) recordSweep(bytes int64, files int) {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	s.bytes, s.files = bytes, int64(files)
}

// Counters returns a copy of the spool's own counters, safe to read while the
// flush goroutine writes. A nil spool is the log-only mode: it has written
// nothing, and says so with every registered kind at zero rather than with
// silence.
func (s *Spool) Counters() Counters {
	if s == nil {
		return Counters{
			Written:       newKindCounts(),
			WriteFailures: newKindCounts(),
			Evicted:       newReasonCounts(),
		}
	}
	s.countMu.Lock()
	defer s.countMu.Unlock()
	return Counters{
		Written:       copyCounts(s.written),
		WriteFailures: copyCounts(s.writeFailures),
		Evicted:       copyCounts(s.evicted),
		Bytes:         s.bytes,
		Files:         s.files,
	}
}

func copyCounts(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// newKindCounts seeds a per-kind map from the registry, so the list of payload
// kinds has exactly one home (ADR 0022) and a kind that never ships is visible
// as a zero rather than as an absence.
func newKindCounts() map[string]int64 {
	out := make(map[string]int64, len(registry))
	for _, k := range registry {
		out[k.Kind] = 0
	}
	return out
}

func newReasonCounts() map[string]int64 {
	out := make(map[string]int64, len(EvictionReasons))
	for _, r := range EvictionReasons {
		out[r] = 0
	}
	return out
}

// SortedKeys returns a map's keys in a stable order, so an exposition built
// from one of these maps does not reorder between scrapes.
func SortedKeys(in map[string]int64) []string {
	out := make([]string, 0, len(in))
	for k := range in {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
