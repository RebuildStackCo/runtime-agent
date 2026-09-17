package decisions

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The names this package answers are the names every other decision in this
// repository works to keep inside the cluster. What stops them from leaving is
// not the handler's code — it is that nothing which can transmit knows this
// package exists (ADR 0084). That is a property of the import graph, so it is
// checked as one.

// transmitters are the packages that can put bytes on the wire or in a spool
// file. None of them may reach what is served here, and this package may not
// reach them.
var transmitters = []string{"internal/sink", "internal/shipper", "internal/journal"}

// importsOf reads every Go file in dir, build tags and all: a file this walk
// skipped would be a file free to import what the rest may not.
func importsOf(t *testing.T, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := make(map[string][]string)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", entry.Name(), err)
		}
		for _, imp := range file.Imports {
			out[entry.Name()] = append(out[entry.Name()], strings.Trim(imp.Path.Value, `"`))
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no Go files; this check would pass vacuously", dir)
	}
	return out
}

func TestNothingThatCanTransmitKnowsThisPackage(t *testing.T) {
	for _, dir := range transmitters {
		for file, imports := range importsOf(t, filepath.Join("..", filepath.Base(dir))) {
			for _, imp := range imports {
				if strings.HasSuffix(imp, "/internal/decisions") {
					t.Errorf("%s/%s imports the decision listing; what it answers names objects "+
						"your filters excluded and must not reach a payload (ADR 0084)", dir, file)
				}
			}
		}
	}
}

func TestThisPackageReachesNothingThatCanTransmit(t *testing.T) {
	for file, imports := range importsOf(t, ".") {
		for _, imp := range imports {
			for _, banned := range transmitters {
				if strings.Contains(imp, banned) {
					t.Errorf("%s imports %s; this listener answers inside the cluster only (ADR 0084)", file, imp)
				}
			}
		}
	}
}
