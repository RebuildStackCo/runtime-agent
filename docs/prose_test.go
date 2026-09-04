package docs

// ADR 0024 cut `security.md` from 750 lines of half-duplicated ADR text and
// stated the rule: this document says what is true, the decision records say
// why. It had no mechanism, and in five days the document was back to 1139
// lines. ADR 0044 extends the rule to code comments and puts the numbers here.
//
// A ceiling is a ratchet, not a target. Raising one is a decision that shows up
// in a diff and has to be argued for, which is the whole difference between
// this and a convention.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A comment longer than this is rationale, and rationale belongs in an ADR with
// a pointer left behind (ADR 0044).
const maxCommentRun = 8

// The two documents that grow with every payload kind, at the size each landed
// once the restated ADRs came out (ADR 0044). `security.md` is deliberately not
// back at ADR 0024's 750: kinds have been added since. `backend-requirements.md`
// restates decisions on purpose — its reader implements ingest and reads neither
// our ADRs nor `security.md` — so what is bounded there is growth, not
// duplication. A ceiling moves only for an obligation a document did not have
// before, never to fit prose that restates one; which obligation is in the diff
// that moved the number.
var ceilings = []struct {
	file    string
	lines   int
	section int
}{
	{"security.md", 1043, 360},
	{"backend-requirements.md", 675, 532},
}

func TestNoCommentRunIsLongerThanAPointerToAnADR(t *testing.T) {
	for _, path := range goFiles(t) {
		// #nosec G304 -- path comes from walking this repository, not from input
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		run, start := 0, 0
		report := func() {
			if run > maxCommentRun {
				t.Errorf("%s:%d: %d comment lines in a row, at most %d; "+
					"move the reasoning to an ADR and cite it (ADR 0044)", path, start, run, maxCommentRun)
			}
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				if run == 0 {
					start = i + 1
				}
				run++
				continue
			}
			report()
			run = 0
		}
		report()
	}
}

func TestTheLongDocumentsStayUnderTheirCeilings(t *testing.T) {
	for _, c := range ceilings {
		// #nosec G304 -- the filename is a literal in the table above
		body, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("reading %s: %v", c.file, err)
		}
		lines := strings.Split(string(body), "\n")
		if len(lines) > c.lines {
			t.Errorf("%s is %d lines, ceiling %d; a section that grows means another shrinks (ADR 0024, ADR 0044)",
				c.file, len(lines), c.lines)
		}

		heading, count := "", 0
		check := func() {
			if heading != "" && count > c.section {
				t.Errorf("%s section %q is %d lines, ceiling %d; the detail belongs in the ADR it cites",
					c.file, heading, count, c.section)
			}
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "## ") {
				check()
				heading, count = strings.TrimPrefix(line, "## "), 0
				continue
			}
			count++
		}
		check()
	}
}

// goFiles returns every Go file in the repository. Tests are included: a test
// file is read more often than the code it covers, not less.
//
// The count is asserted because the first version of this walk found nothing:
// the repository root is reached as ".." and its Name() is "..", which the
// dot-prefix skip below matched, so the whole tree was pruned at the first entry
// and the test passed on an empty list.
func goFiles(t *testing.T) []string {
	t.Helper()
	const root = ".."
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "bin") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
	if len(out) < 50 {
		t.Fatalf("the walk found %d Go files; the repository has far more, so the walk is broken rather than the code clean", len(out))
	}
	return out
}
