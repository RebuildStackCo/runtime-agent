package nodescan

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkerIsFoundAcrossAChunkBoundary(t *testing.T) {
	marker := []byte(pprofMarker)
	// Place the marker so that it starts one byte before the end of the first
	// chunk: a scanner without overlap sees neither half.
	prefix := bytes.Repeat([]byte{0x00}, scanChunkBytes-1)
	body := append(append(prefix, marker...), bytes.Repeat([]byte{0x00}, 4096)...)

	if !scanForMarker(bytes.NewReader(body)) {
		t.Error("marker straddling a chunk boundary was not found")
	}
}

// The name is matched whole because "pprof" alone appears in things that are not
// the package — a recorded source path, for one, which is how a binary built in
// a directory named after the package would otherwise report an endpoint.
func TestOnlyTheWholeFunctionNameCounts(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"the function name", "…" + pprofMarker + "…", true},
		{"a source path mentioning the package", "/src/pprofdetect/main.go", false},
		{"the import path alone", "net/http/pprof", false},
		{"nothing like it", strings.Repeat("runtime.main", 100), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scanForMarker(strings.NewReader(tc.body)); got != tc.want {
				t.Errorf("scan = %v, want %v", got, tc.want)
			}
		})
	}
}

// The claim the whole detection funnel rests on: the marker is present exactly
// when the package is imported, and the linker keeps it even when the build
// strips the symbol table — which production builds do.
func TestTheMarkerSurvivesTheLinker(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no toolchain")
	}
	sources := map[string]string{
		"with": `package main

import (
	"net/http"
	_ "net/http/pprof"
)

func main() { _ = http.ListenAndServe(":6060", nil) }
`,
		"without": `package main

import "net/http"

func main() { _ = http.ListenAndServe(":8080", nil) }
`,
	}
	for _, stripped := range []bool{false, true} {
		for name, source := range sources {
			dir := t.TempDir()
			writeFixture(t, filepath.Join(dir, "go.mod"), "module detect\n\ngo 1.25\n")
			writeFixture(t, filepath.Join(dir, "main.go"), source)
			bin := filepath.Join(dir, "bin")

			args := []string{"build", "-o", bin}
			if stripped {
				args = append(args, "-ldflags=-s -w")
			}
			cmd := exec.Command("go", append(args, ".")...) // #nosec G204 -- fixed arguments
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("building %s: %v\n%s", name, err, out)
			}

			f, err := os.Open(bin) // #nosec G304 -- test-controlled path
			if err != nil {
				t.Fatal(err)
			}
			got := scanForMarker(f)
			_ = f.Close()
			if want := name == "with"; got != want {
				t.Errorf("%s binary (stripped=%v): marker present = %v, want %v", name, stripped, got, want)
			}
		}
	}
}

// The cache is what keeps the cost of this per distinct binary rather than per
// process per pass, and forgetting is what keeps it bounded by what runs.
func TestTheMarkerCacheForgetsBinariesNoProcessRuns(t *testing.T) {
	dir := t.TempDir()
	linked := filepath.Join(dir, "linked")
	plain := filepath.Join(dir, "plain")
	writeFixture(t, linked, "…"+pprofMarker+"…")
	writeFixture(t, plain, "nothing to see")

	c := newMarkCache()
	c.startPass()
	if !c.lookup(linked) || c.lookup(plain) {
		t.Fatal("first pass answered wrongly")
	}
	c.endPass()
	if len(c.answers) != 2 {
		t.Fatalf("cache holds %d answers after a pass that saw two binaries, want 2", len(c.answers))
	}

	c.startPass()
	c.lookup(linked)
	c.endPass()
	if len(c.answers) != 1 {
		t.Fatalf("cache holds %d answers after a pass that saw one binary, want 1", len(c.answers))
	}
}

func TestAnUnreadableBinaryClaimsNoEndpoint(t *testing.T) {
	c := newMarkCache()
	c.startPass()
	if c.lookup(filepath.Join(t.TempDir(), "gone")) {
		t.Error("an unreadable executable was reported as linking the package")
	}
}
