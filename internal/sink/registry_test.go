package sink

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The registry is checked against the golden payload bytes rather than against
// the writers, because the bytes are what the backend receives. A writer whose
// constant disagrees with its golden already fails the golden test; what these
// tests add is the two directions no golden can check on its own — a kind that
// ships with no row, and a row that describes nothing that ships.
//
// This is the mechanism ADR 0022 exists for. The registry lived in a document
// for nine slices and drifted in three rows without anything failing.

// goldenEnvelope is the part of every payload the registry describes.
type goldenEnvelope struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
}

// shippedKinds reads the kind and source out of every golden payload.
func shippedKinds(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*.golden.json"))
	if err != nil {
		t.Fatalf("globbing goldens: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no golden payloads found; the registry check would pass vacuously")
	}

	shipped := make(map[string]string, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path) // #nosec G304 -- test-controlled path
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		var env goldenEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		if env.Kind == "" {
			t.Errorf("%s declares no kind; every payload must (ADR 0012 §1)", filepath.Base(path))
			continue
		}
		if prev, dup := shipped[env.Kind]; dup {
			t.Errorf("kind %q appears in two goldens (source %q and %q); a kind has one shape",
				env.Kind, prev, env.Source)
			continue
		}
		shipped[env.Kind] = env.Source
	}
	return shipped
}

// A payload that ships must have a registry row, and its provenance must be the
// one the row declares. This is the direction that catches a new kind invented
// at a call site — the practice ADR 0012 §1 forbade and nothing enforced.
func TestEveryShippedKindIsRegistered(t *testing.T) {
	for kind, source := range shippedKinds(t) {
		entry, ok := Lookup(kind)
		if !ok {
			t.Errorf("payload kind %q ships but has no registry row; add one in registry.go "+
				"and record the decision in an ADR", kind)
			continue
		}
		if entry.Source != source {
			t.Errorf("kind %q ships source %q, registry says %q", kind, source, entry.Source)
		}
	}
}

// And the reverse: a row that describes nothing is a registry that has started
// drifting away from the artifact, which is the failure this whole mechanism
// exists to catch — in the direction it actually happened.
func TestEveryRegisteredKindShips(t *testing.T) {
	shipped := shippedKinds(t)
	for _, entry := range Registry() {
		if _, ok := shipped[entry.Kind]; !ok {
			t.Errorf("registry row %q has no golden payload; either it no longer ships "+
				"(remove the row) or it ships untested (add the golden)", entry.Kind)
		}
	}
}

// Provenance is a closed vocabulary (ADR 0012 §2): the backend switches on it,
// so a fifth class invented at a call site would be silently unhandled there.
func TestRegistrySourcesAreKnownProvenanceClasses(t *testing.T) {
	known := map[string]bool{
		SourceStructural: true,
		SourceMeasured:   true,
		SourceJournal:    true,
		SourceSampled:    true,
	}
	for _, entry := range Registry() {
		if !known[entry.Source] {
			t.Errorf("kind %q declares source %q, which is not a provenance class", entry.Kind, entry.Source)
		}
	}
}

// Every row must be complete. A row missing its key or its delivery discipline
// describes a payload the backend cannot ingest correctly, and the omission is
// invisible until someone tries.
func TestRegistryRowsAreComplete(t *testing.T) {
	for _, entry := range Registry() {
		if entry.NaturalKey == "" {
			t.Errorf("kind %q has no natural key", entry.Kind)
		}
		if entry.ADR == "" {
			t.Errorf("kind %q names no ADR", entry.Kind)
		}
		switch entry.Delivery {
		case DeliverySupersedes, DeliveryAccumulates, DeliveryWriteOnce:
		default:
			t.Errorf("kind %q has delivery %q, which is not a discipline the backend implements",
				entry.Kind, entry.Delivery)
		}
	}
}

// No payload carries an ordering field (ADR 0027). The five counters that
// produced `sequence` restarted at one with the process, so a backend obeying
// the old "order by the agent's sequence" rule would have rejected everything a
// restarted controller sent under a fixed key until the count caught up.
//
// The replacement is structural rather than declared: the spool holds one
// version of a natural key, atomically replaced, so last-write-wins is correct
// without a field. This test is what stops a future kind from quietly
// reintroducing one — the reasoning lives in an ADR, and an ADR cannot fail.
func TestNoPayloadCarriesAnOrderingField(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.golden.json"))
	if err != nil {
		t.Fatalf("globbing goldens: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no golden payloads found; this check would pass vacuously")
	}
	// Names a payload must not use for an ordering field. `captured_at` is not
	// here on purpose: it dates an observation, and ADR 0027 §3 keeps it a fact
	// rather than machinery.
	ordering := []string{"sequence", "seq", "revision", "generation", "version"}
	for _, path := range paths {
		raw, err := os.ReadFile(path) // #nosec G304 -- test-controlled path
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, name := range ordering {
			if _, found := fields[name]; found {
				t.Errorf("%s carries %q. Superseding payloads are ordered by nothing: the "+
					"spool holds one version of a key (ADR 0027). If a total order is really "+
					"needed, it takes its own ADR — a counter cannot survive a restart.",
					filepath.Base(path), name)
			}
		}
	}
}
