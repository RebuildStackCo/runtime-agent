package metrics

import (
	"sort"
	"strings"
	"testing"
)

// TestOnlyTheseLabelNamesAreAllowed pins the closed set. A label name added to
// the package without a line here fails, which is the point: the set is a
// cardinality budget and the continuation of "identities do not leave by a
// second channel" at once (CLAUDE.md invariant 6, ADR 0070 §3).
func TestOnlyTheseLabelNamesAreAllowed(t *testing.T) {
	want := []string{
		"kind", "outcome", "path", "reason", "role", "source", "state",
		"subject", "version",
	}
	var got []string
	for name := range Allowed {
		got = append(got, string(name))
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("permitted label names are %v, pinned as %v; widening the set is a decision, "+
			"and one that must not admit a namespace, pod, node, workload or image name", got, want)
	}
}

// TestALabelOutsideTheSetYieldsNoExposition is the runtime half. The test above
// is what fails a build; this is what the endpoint does if one ever slips past
// it — serve nothing rather than a series naming something it should not.
func TestALabelOutsideTheSetYieldsNoExposition(t *testing.T) {
	for _, name := range []LabelName{"namespace", "pod", "node", "workload", "image", ""} {
		s := NewSet()
		s.Gauge("pods_observed", "help", 1, L(name, "anything"))
		s.Gauge("spool_files", "help", 3)
		families, err := s.Families()
		if err == nil {
			t.Errorf("label %q was accepted", name)
		}
		if families != nil {
			t.Errorf("label %q left %d families exposed; a refusal exposes nothing", name, len(families))
		}
	}
}

func TestEveryNameCarriesThePrefix(t *testing.T) {
	s := NewSet()
	s.Gauge("spool_files", "help", 1)
	s.Counter("pods_observed_total", "help", 2)
	families, err := s.Families()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), Prefix) {
			t.Errorf("metric %q does not carry the %q prefix", f.GetName(), Prefix)
		}
	}
}

// TestOneNameIsOneFamily: repeated adds under one name become one family with
// many series, which is what the text format requires.
func TestOneNameIsOneFamily(t *testing.T) {
	s := NewSet()
	s.Counter("pods_excluded_total", "help", 1, L(LabelReason, "namespace_filter"))
	s.Counter("pods_excluded_total", "help", 2, L(LabelReason, "pod_annotation"))
	families, err := s.Families()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 {
		t.Fatalf("got %d families, want 1", len(families))
	}
	if n := len(families[0].GetMetric()); n != 2 {
		t.Errorf("got %d series, want 2", n)
	}
}

// TestOneNameCannotBeTwoTypes: a name exposed as both a counter and a gauge is
// an exposition no scraper can read, so it is refused rather than served.
func TestOneNameCannotBeTwoTypes(t *testing.T) {
	s := NewSet()
	s.Counter("spool_evicted_total", "help", 1, L(LabelReason, "age"))
	s.Gauge("spool_evicted_total", "help", 1, L(LabelReason, "bytes"))
	if _, err := s.Families(); err == nil {
		t.Error("a name exposed as two types was accepted")
	}
}

func TestFamiliesAreOrderedStably(t *testing.T) {
	s := NewSet()
	s.Gauge("spool_files", "help", 1)
	s.Gauge("build_info", "help", 1, L(LabelRole, RoleNode), L(LabelVersion, "dev"))
	s.Gauge("nodes_reporting", "help", 1)
	families, err := s.Families()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range families {
		names = append(names, f.GetName())
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("families are not in a stable order: %v", names)
	}
}

// TestLabelsAreOrderedWithinASeries keeps the bytes of one scrape comparable to
// the next, which a map's iteration order would not.
func TestLabelsAreOrderedWithinASeries(t *testing.T) {
	s := NewSet()
	s.Gauge("build_info", "help", 1, L(LabelVersion, "1.2.3"), L(LabelRole, RoleController))
	families, err := s.Families()
	if err != nil {
		t.Fatal(err)
	}
	labels := families[0].GetMetric()[0].GetLabel()
	if len(labels) != 2 || labels[0].GetName() != "role" || labels[1].GetName() != "version" {
		t.Errorf("labels are not sorted by name: %v", labels)
	}
}
