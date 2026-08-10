// Package sink writes payload batches to the local spool. The local sink is
// the schema contract: it writes exactly the bytes the backend sink will
// transmit (docs/development.md), so the golden files over its output are
// the enforcement mechanism for the payload schema. The spool directory's
// durability is an installation choice, not a requirement (ADR 0007); its
// layout follows ADR 0003 — filenames are natural keys, deletion is
// acknowledgment, no index.
package sink

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/inventory"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// DefaultMaxAge caps how long an unacknowledged payload may sit in the
// spool. Past it, files are removed even without acknowledgment: an
// extended outage degrades to memory-only behavior instead of filling the
// volume (ADR 0007).
const DefaultMaxAge = 24 * time.Hour

// Spool writes payload files into one directory. Methods may be called from
// different goroutines as long as no two writers target the same natural
// key — which the agent's wiring guarantees (one usage poller, one OOM
// reporter).
type Spool struct {
	dir    string
	maxAge time.Duration
}

// NewSpool opens (creating if needed) the spool directory. maxAge ≤ 0
// selects DefaultMaxAge.
func NewSpool(dir string, maxAge time.Duration) (*Spool, error) {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating spool directory: %w", err)
	}
	return &Spool{dir: dir, maxAge: maxAge}, nil
}

// usagePayload is one shippable batch: every record of one wall-clock
// window. Kind "usage_snapshot" is an open-window snapshot that supersedes
// its predecessor; "usage_window" is the final record of a closed window.
type usagePayload struct {
	Kind          string           `json:"kind"`
	Sequence      int64            `json:"sequence,omitempty"`
	WindowStart   time.Time        `json:"window_start"`
	WindowSeconds int64            `json:"window_seconds"`
	Records       []*rollup.Record `json:"records"`
}

// oomPayload is one OOM kill event; events bypass windows and ship
// immediately (ADR 0006).
type oomPayload struct {
	Kind  string            `json:"kind"`
	Event collector.OOMKill `json:"event"`
}

// goInventoryPayload is the current Go inventory of the cluster — one record
// per (namespace, workload, container), joined from node facts (ADR 0010). It
// is a single superseding batch: each write replaces the previous one under its
// fixed natural key (the payload kind), the on-disk mirror of the backend's
// upsert-by-key ingest. The sequence orders supersedes, never arrival time.
type goInventoryPayload struct {
	Kind     string               `json:"kind"`
	Sequence int64                `json:"sequence,omitempty"`
	Records  []inventory.GoRecord `json:"records"`
}

type windowKey struct {
	start   time.Time
	seconds int64
}

func groupByWindow(records []*rollup.Record) map[windowKey][]*rollup.Record {
	grouped := make(map[windowKey][]*rollup.Record)
	for _, r := range records {
		k := windowKey{start: r.WindowStart, seconds: r.WindowSeconds}
		grouped[k] = append(grouped[k], r)
	}
	return grouped
}

func (w windowKey) name() string {
	return fmt.Sprintf("usage-%d-%d", w.start.Unix(), w.seconds)
}

// WriteUsageSnapshot writes open-window snapshots, one file per window,
// atomically replacing that window's previous snapshot — the on-disk mirror
// of the backend's supersede-by-key ingest. It also sweeps expired files,
// riding the snapshot cadence so no extra timer exists.
func (s *Spool) WriteUsageSnapshot(sequence int64, records []*rollup.Record) error {
	for k, group := range groupByWindow(records) {
		payload := usagePayload{
			Kind:          "usage_snapshot",
			Sequence:      sequence,
			WindowStart:   k.start,
			WindowSeconds: k.seconds,
			Records:       group,
		}
		if err := s.write(k.name()+".snapshot.json", payload); err != nil {
			return err
		}
	}
	return s.sweep(time.Now())
}

// WriteClosedWindows writes final closed-window records, one file per
// window, and removes the window's snapshot file — the closed record
// supersedes every snapshot.
func (s *Spool) WriteClosedWindows(records []*rollup.Record) error {
	for k, group := range groupByWindow(records) {
		payload := usagePayload{
			Kind:          "usage_window",
			WindowStart:   k.start,
			WindowSeconds: k.seconds,
			Records:       group,
		}
		if err := s.write(k.name()+".json", payload); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(s.dir, k.name()+".snapshot.json")); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing superseded snapshot: %w", err)
		}
	}
	return nil
}

// WriteOOMKill writes one OOM event immediately. The filename carries the
// event's natural identity, so a re-observed event overwrites rather than
// duplicates.
func (s *Spool) WriteOOMKill(e collector.OOMKill) error {
	name := fmt.Sprintf("oom-%d-%s-%s-%s-%d.json",
		e.FinishedAt.Unix(), e.Namespace, e.Pod, e.Container, e.RestartCount)
	return s.write(name, oomPayload{Kind: "oom_kill", Event: e})
}

// WriteGoInventory writes the current Go inventory as one superseding batch
// (ADR 0010). One file per cluster, atomically replaced each flush: the newest
// inventory supersedes its predecessor, exactly as an open-window usage
// snapshot does. records must already be in a deterministic order (the store
// sorts them) so the payload bytes are stable — the golden contract.
func (s *Spool) WriteGoInventory(sequence int64, records []inventory.GoRecord) error {
	payload := goInventoryPayload{
		Kind:     "go_inventory",
		Sequence: sequence,
		Records:  records,
	}
	return s.write("go-inventory.json", payload)
}

// write marshals the payload and lands it atomically: temp file in the same
// directory, then rename. A crash mid-write leaves a *.tmp file that the
// sweep collects; readers never observe a partial payload.
func (s *Spool) write(name string, payload any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding payload %s: %w", name, err)
	}
	data = append(data, '\n')
	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing payload %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, name)); err != nil {
		return fmt.Errorf("publishing payload %s: %w", name, err)
	}
	return nil
}

// sweep deletes payloads older than the maximum age, and any temp files a
// crash left behind.
func (s *Spool) sweep(now time.Time) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("listing spool: %w", err)
	}
	cutoff := now.Add(-s.maxAge)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue // raced with a rename or delete — the next sweep sees the truth
		}
		if info.ModTime().Before(cutoff) || (strings.HasSuffix(entry.Name(), ".tmp") && info.ModTime().Before(now.Add(-time.Minute))) {
			if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("sweeping spool: %w", err)
			}
		}
	}
	return nil
}
