// Package docs holds no code. It exists so that the promises in
// `security.md` can be checked the way everything else in this repository is
// checked: by a test that fails.
//
// `security.md` is the promise to customers about what the agent can access and
// what leaves the cluster, and until now it was the only document in the tree
// with no mechanism behind it. An audit found the predictable result — three of
// the ten payload kinds were never named in the section that claims to
// enumerate what leaves, and a cross-reference pointed at a section that did not
// exist. Both are mechanical, so both are checked here (ADR 0022).
//
// What is deliberately not checked: whether the prose is accurate. No test can
// read "the agent never reads Secrets" and confirm it. What a test can hold is
// the part that is a list — and the list is exactly where the drift was.
package docs

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

func securityDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("security.md")
	if err != nil {
		t.Fatalf("reading security.md: %v", err)
	}
	return string(raw)
}

// payloadTableHeading opens the one table in security.md that claims to be the
// complete list of what leaves the cluster.
const payloadTableHeading = "### Every payload the agent produces"

// payloadTableKinds reads the first column of that table.
func payloadTableKinds(t *testing.T) map[string]bool {
	t.Helper()
	doc := securityDoc(t)
	start := strings.Index(doc, payloadTableHeading)
	if start < 0 {
		t.Fatalf("security.md has no %q section; §8's enumeration is what this test holds", payloadTableHeading)
	}
	section := doc[start+len(payloadTableHeading):]
	if end := strings.Index(section, "\n### "); end >= 0 {
		section = section[:end]
	}

	rowRe := regexp.MustCompile("(?m)^\\|\\s*`([a-z_]+)`\\s*\\|")
	kinds := map[string]bool{}
	for _, m := range rowRe.FindAllStringSubmatch(section, -1) {
		kinds[m[1]] = true
	}
	if len(kinds) == 0 {
		t.Fatal("the payload table has no rows; this check would pass vacuously")
	}
	return kinds
}

// The table and the registry must be the same set, in both directions. §8 is
// the section a security review reads to learn what leaves the cluster, so a
// kind missing from it is an undisclosed payload, and a kind listed there that
// nothing ships is a capability the agent does not have.
//
// This is the direction that actually failed: `usage_snapshot`, `usage_window`
// and `ebpf_profile` shipped for months while the document described the first
// two only as "hourly rollup histograms" and never named the third, so a
// reviewer could not map the document onto the wire.
func TestSecurityDocPayloadTableMirrorsTheRegistry(t *testing.T) {
	listed := payloadTableKinds(t)

	shipped := map[string]bool{}
	for _, entry := range sink.Registry() {
		shipped[entry.Kind] = true
		if !listed[entry.Kind] {
			t.Errorf("payload kind %q ships but is not in security.md's payload table; "+
				"§8 claims to enumerate everything that leaves the cluster", entry.Kind)
		}
	}
	for kind := range listed {
		if !shipped[kind] {
			t.Errorf("security.md's payload table lists %q, which nothing ships; "+
				"either the row is stale or the registry is missing one", kind)
		}
	}
}

var (
	headingRe = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	anchorRe  = regexp.MustCompile(`\]\(#([a-z0-9-]+)\)`)
	slugDrop  = regexp.MustCompile(`[^a-z0-9 -]`)
)

// slug renders a heading the way GitHub does: lowercase, punctuation dropped,
// spaces to hyphens.
func slug(heading string) string {
	s := strings.ToLower(strings.TrimSpace(heading))
	s = slugDrop.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, " ", "-")
}

// A cross-reference that resolves to nothing is worse than none: it tells a
// reviewer their answer is elsewhere in the document and then does not take
// them there. §8 pointed at "§11 your controls" while §11 was called something
// else and listed no controls.
func TestSecurityDocAnchorsResolve(t *testing.T) {
	doc := securityDoc(t)

	anchors := map[string]bool{}
	for _, m := range headingRe.FindAllStringSubmatch(doc, -1) {
		anchors[slug(m[1])] = true
	}
	for _, m := range anchorRe.FindAllStringSubmatch(doc, -1) {
		if !anchors[m[1]] {
			t.Errorf("security.md links to #%s, which no heading produces", m[1])
		}
	}
}

// The diet of this document moved reasoning into the ADRs and left links behind.
// A link to a decision record that does not exist would put the reasoning
// nowhere at all — and the repository README, which is the front door, carries
// the same kind of link.
func TestADRLinksExist(t *testing.T) {
	for _, doc := range []struct{ name, prefix string }{
		{"security.md", ""},
		{"../README.md", "docs/"},
	} {
		raw, err := os.ReadFile(doc.name) // #nosec G304 -- test-controlled path
		if err != nil {
			t.Fatalf("reading %s: %v", doc.name, err)
		}
		re := regexp.MustCompile(`\]\(` + doc.prefix + `(adr/[0-9a-z-]+\.md)\)`)
		found := false
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			found = true
			if _, err := os.Stat(m[1]); err != nil { // #nosec G703 -- the regex admits only adr/<slug>.md
				t.Errorf("%s links %s%s, which does not exist", doc.name, doc.prefix, m[1])
			}
		}
		if !found {
			t.Errorf("%s links no decision record; the reasoning has to be reachable from it", doc.name)
		}
	}
}
