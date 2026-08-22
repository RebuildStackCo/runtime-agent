// Package adr holds no code. It exists so that the decision records in this
// directory can be checked the way everything else in this repository is
// checked: by a test that fails.
//
// The problem this answers is recorded in ADR 0022. Backward references — "this
// amends 0012 §1" — were already written in almost every ADR header, in two
// incompatible prose shapes, and two ADRs that amended others said nothing at
// all. What was missing was the other direction: a reader arriving at ADR 0012
// saw a registry table six later decisions had rewritten, with no sign that
// anything had happened to it. Planning a slice from that table is how three
// separate inaccuracies reached merged pull requests.
//
// So the header carries a machine-readable `Amends:` line, and the mirror
// `Amended by:` line on the target is checked here. Neither can drift: an ADR
// that amends another cannot stay silent about it, and the amended ADR cannot
// stay unaware.
package adr

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

var (
	headingRe    = regexp.MustCompile(`^#\s+(\d{4})\.\s`)
	statusRe     = regexp.MustCompile(`^Status:\s*(.+)$`)
	amendsRe     = regexp.MustCompile(`^Amends:\s*(.+)$`)
	amendedByRe  = regexp.MustCompile(`^Amended by:\s*(.+)$`)
	supersededRe = regexp.MustCompile(`^Superseded by\s+(\d{4})\b`)
	numberRe     = regexp.MustCompile(`\b(\d{4})\b`)
)

// record is one ADR's header: everything this test reasons about.
type record struct {
	number    string
	file      string
	status    string
	amends    []string
	amendedBy []string
}

// headerLines is how far into a file the header may reach. Everything this test
// reads sits above the first section; scanning the whole body would let a
// number in prose look like a declaration.
const headerLines = 12

// load reads every ADR's header. The body is deliberately not parsed: an
// accepted ADR's body is immutable (README), so nothing in it can be a live
// declaration about the graph.
func load(t *testing.T) map[string]*record {
	t.Helper()
	paths, err := filepath.Glob("0*.md")
	if err != nil {
		t.Fatalf("globbing ADRs: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no ADRs found; these checks would pass vacuously")
	}

	byNumber := make(map[string]*record, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path) // #nosec G304 -- test-controlled path
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		lines := strings.Split(string(raw), "\n")

		rec := &record{file: path}
		for i, line := range lines {
			if i >= headerLines {
				break
			}
			line = strings.TrimSpace(line)
			switch {
			case headingRe.MatchString(line):
				rec.number = headingRe.FindStringSubmatch(line)[1]
			case statusRe.MatchString(line):
				rec.status = strings.TrimSpace(statusRe.FindStringSubmatch(line)[1])
			case amendsRe.MatchString(line):
				rec.amends = numbers(amendsRe.FindStringSubmatch(line)[1])
			case amendedByRe.MatchString(line):
				rec.amendedBy = numbers(amendedByRe.FindStringSubmatch(line)[1])
			}
		}

		if rec.number == "" {
			t.Errorf("%s has no `# NNNN. Title` heading in its first %d lines", path, headerLines)
			continue
		}
		if prefix := rec.number + "-"; !strings.HasPrefix(path, prefix) {
			t.Errorf("%s declares itself ADR %s; filename and heading must agree", path, rec.number)
		}
		if prev, dup := byNumber[rec.number]; dup {
			t.Errorf("ADR %s is claimed by both %s and %s", rec.number, prev.file, path)
			continue
		}
		byNumber[rec.number] = rec
	}
	return byNumber
}

// numbers pulls the four-digit ADR numbers out of a header field, so a field may
// carry section detail — "0012 §1, 0017" — and stay readable.
func numbers(field string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range numberRe.FindAllStringSubmatch(field, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// The graph must be symmetric. This is the check that would have caught every
// stale premise this mechanism was built for: ADR 0012's table rewritten six
// times with no sign of it in the file.
func TestAmendmentGraphIsSymmetric(t *testing.T) {
	adrs := load(t)

	for number, rec := range adrs {
		for _, target := range rec.amends {
			if target == number {
				t.Errorf("ADR %s amends itself", number)
				continue
			}
			other, ok := adrs[target]
			if !ok {
				t.Errorf("%s amends ADR %s, which does not exist", rec.file, target)
				continue
			}
			if !slices.Contains(other.amendedBy, number) {
				t.Errorf("%s amends ADR %s, but %s does not say so.\n"+
					"Add %s to its header:\n    Amended by: %s",
					rec.file, target, other.file, number, strings.Join(merged(other.amendedBy, number), ", "))
			}
		}
	}

	for number, rec := range adrs {
		for _, claimed := range rec.amendedBy {
			other, ok := adrs[claimed]
			if !ok {
				t.Errorf("%s says ADR %s amends it, but %s does not exist", rec.file, claimed, claimed)
				continue
			}
			if !slices.Contains(other.amends, number) {
				t.Errorf("%s claims to be amended by ADR %s, but %s does not declare `Amends: %s`",
					rec.file, claimed, other.file, number)
			}
		}
	}
}

// An amendment always points backward in time. A lower-numbered ADR amending a
// higher-numbered one would mean an accepted decision was edited after the fact,
// which the immutability rule forbids.
func TestAmendmentsPointBackwards(t *testing.T) {
	for number, rec := range load(t) {
		for _, target := range rec.amends {
			if target >= number {
				t.Errorf("%s amends ADR %s, which is not older; an accepted ADR is never "+
					"edited by a later one, it is amended by it", rec.file, target)
			}
		}
	}
}

// Status is a closed vocabulary: a decision is in force, or a named later
// decision replaced it whole. Anything else is prose pretending to be metadata —
// which is exactly what the old free-form `Status: Accepted (amends 0006's
// delivery consequences; refines 0003's volume assumption)` was.
func TestStatusIsAcceptedOrSuperseded(t *testing.T) {
	adrs := load(t)
	for number, rec := range adrs {
		switch {
		case rec.status == "Accepted":
		case supersededRe.MatchString(rec.status):
			target := supersededRe.FindStringSubmatch(rec.status)[1]
			if _, ok := adrs[target]; !ok {
				t.Errorf("%s is superseded by ADR %s, which does not exist", rec.file, target)
			}
			if target <= number {
				t.Errorf("%s is superseded by ADR %s, which is not later", rec.file, target)
			}
		case rec.status == "":
			t.Errorf("%s has no Status line", rec.file)
		default:
			t.Errorf("%s has Status %q; want `Accepted` or `Superseded by NNNN`. "+
				"Partial amendment goes in the `Amends:` field, not in the status",
				rec.file, rec.status)
		}
	}
}

// The index is how anyone finds an ADR. One missing line and a decision is
// invisible to everybody who did not already know it existed.
func TestIndexListsEveryADR(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	index := string(raw)

	adrs := load(t)
	for number, rec := range adrs {
		if !strings.Contains(index, "("+rec.file+")") {
			t.Errorf("ADR %s (%s) is not linked from the index in README.md", number, rec.file)
		}
	}

	for _, m := range regexp.MustCompile(`\((\d{4}-[a-z0-9-]+\.md)\)`).FindAllStringSubmatch(index, -1) {
		// #nosec G703 -- the regex admits only NNNN-slug.md, so no separator and
		// no traversal can reach here; the check is that the link resolves.
		if _, err := os.Stat(m[1]); err != nil {
			t.Errorf("the index links %s, which does not exist", m[1])
		}
	}
}

// merged is only used to render a helpful failure message: the line the author
// should paste, already sorted.
func merged(existing []string, add string) []string {
	out := append(append([]string{}, existing...), add)
	sort.Strings(out)
	return out
}
