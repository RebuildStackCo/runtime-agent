package model_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The package's purpose is that a collected fact carries no cluster dependency
// (ADR 0065). One k8s.io import here would put client-go back into every
// package that windows or serializes a fact — the state this package exists to
// leave — and it would do so silently, since nothing else in the build fails.
// The rule is therefore checked rather than remembered.
func TestModelImportsNothingOutsideTheStandardLibrary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, imported := range file.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			// A module path begins with a domain and a standard-library path
			// does not — the same test nodeprofile's symbol filter makes.
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %q; a collected fact is plain data and this package holds no dependency", name, path)
			}
		}
	}
}
