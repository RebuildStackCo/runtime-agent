// Package image holds no code. It checks the two files that decide what the
// shipped container is made of — the Dockerfile and the build context — the way
// `internal/chartrender` checks the chart: by a test that fails (ADR 0085).
package image

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const (
	dockerfile   = "../../Dockerfile"
	dockerignore = "../../.dockerignore"
	gomod        = "../../go.mod"
)

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) // #nosec G304 -- the path is a constant above
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(body)
}

var fromRe = regexp.MustCompile(`(?m)^FROM\s+(\S+)`)

// A tag is a name its owner may move. Both bases are pinned by digest so that a
// rebuild of a commit is a rebuild of the same bytes, and so that a moved tag
// arrives as a Dependabot pull request that CI builds and scans rather than as
// a silent change in what we ship.
func TestEveryBaseImageIsPinnedByDigest(t *testing.T) {
	bases := fromRe.FindAllStringSubmatch(read(t, dockerfile), -1)
	if len(bases) < 2 {
		t.Fatalf("found %d FROM lines; the build has a builder and a runtime stage", len(bases))
	}
	for _, base := range bases {
		if !strings.Contains(base[1], "@sha256:") {
			t.Errorf("%q is pinned by tag alone; a tag is a name its owner may move (ADR 0085)", base[1])
		}
	}
}

// ADR 0038 §3 moved the toolchain floor in `go.mod` and in the build image
// together, and said why: they are the same fact, and drifting them apart means
// CI proving something about a binary the release does not build. An automated
// bump of one of the two is exactly how that drift would now arrive.
func TestTheBuilderAndGoModAgreeOnTheToolchain(t *testing.T) {
	var declared string
	for _, line := range strings.Split(read(t, gomod), "\n") {
		if rest, ok := strings.CutPrefix(line, "go "); ok {
			declared = strings.TrimSpace(rest)
			break
		}
	}
	if declared == "" {
		t.Fatal("go.mod declares no go directive")
	}

	builder := fromRe.FindStringSubmatch(read(t, dockerfile))
	if builder == nil {
		t.Fatal("the Dockerfile has no FROM line")
	}
	want := "golang:" + declared + "@"
	if !strings.HasPrefix(builder[1], want) {
		t.Errorf("go.mod says Go %s and the build image is %q; the toolchain floor is one fact (ADR 0038 §3)",
			declared, builder[1])
	}
}

// The context is what a build could publish. Private notes must not be in it,
// and `.git` must be — it is what stamps vcs.revision and vcs.modified into the
// binary, which is the provenance this agent reads off customer workloads.
func TestTheBuildContextHoldsProvenanceAndNoPrivateNotes(t *testing.T) {
	var entries []string
	for _, line := range strings.Split(read(t, dockerignore), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			entries = append(entries, line)
		}
	}

	var excludesNotes bool
	for _, entry := range entries {
		if entry == "CONTEXT.md" {
			excludesNotes = true
		}
		if entry == ".git" || entry == ".git/" || entry == "**/.git" {
			t.Errorf("the build context excludes %q; the binary would then carry no vcs.revision, "+
				"and an image of ours that cannot answer where it came from is the one thing we tell "+
				"customers to look for (ADR 0085)", entry)
		}
	}
	if !excludesNotes {
		t.Error("the build context does not exclude CONTEXT.md; a build context is an artifact " +
			"the moment a cache or an intermediate stage is exported (CLAUDE.md)")
	}
}
