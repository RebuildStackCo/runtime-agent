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

const (
	// A comment longer than this is rationale, and rationale belongs in an ADR
	// with a pointer left behind (ADR 0044).
	maxCommentRun = 8

	// Where `security.md` and its largest section landed once the restated ADRs
	// were removed. Not ADR 0024's 750: five payload kinds have been added since
	// (ADR 0029, 0030, 0032, 0034), and each is a row in the disclosure table and
	// a paragraph under it. The rest of §8 is what a security review reads.
	maxSecurityLines   = 920
	maxSecuritySection = 290
)

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

func TestTheSecurityDocumentStaysUnderItsCeiling(t *testing.T) {
	body, err := os.ReadFile("security.md")
	if err != nil {
		t.Fatalf("reading security.md: %v", err)
	}
	lines := strings.Split(string(body), "\n")
	if len(lines) > maxSecurityLines {
		t.Errorf("security.md is %d lines, ceiling %d; a section that grows means another shrinks (ADR 0024, ADR 0044)",
			len(lines), maxSecurityLines)
	}

	heading, count := "", 0
	check := func() {
		if heading != "" && count > maxSecuritySection {
			t.Errorf("security.md section %q is %d lines, ceiling %d; the customer reads this document, "+
				"so the detail belongs in the ADR it cites", heading, count, maxSecuritySection)
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
