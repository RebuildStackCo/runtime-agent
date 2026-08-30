package nodeprofile

import (
	"reflect"
	"testing"
)

// A pass replaces the last one wholesale, so a container no live process belongs
// to leaves with it. Accumulating instead would keep answering for pods that are
// gone, which is the staleness ADR 0052 §3 settled for peaks.
func TestAPassReplacesTheIndexRatherThanAddingToIt(t *testing.T) {
	var idx ModuleIndex
	idx.Publish(map[string][]string{"cid-a": {"github.com/acme/a"}, "cid-b": {"github.com/acme/b"}})
	if idx.Size() != 2 {
		t.Fatalf("size = %d after a pass that saw two containers, want 2", idx.Size())
	}

	idx.Publish(map[string][]string{"cid-a": {"github.com/acme/a"}})
	if idx.Size() != 1 {
		t.Errorf("size = %d after a pass that saw one, want 1", idx.Size())
	}
	if got := idx.Modules("cid-b"); got != nil {
		t.Errorf("modules for a departed container = %v, want none", got)
	}
}

// Before the first pass there is nothing, and that must be an empty answer
// rather than a panic: the first capture window can outrun the first scan.
func TestAnUnpublishedIndexAnswersNothing(t *testing.T) {
	var idx ModuleIndex
	if idx.Size() != 0 || idx.Modules("cid") != nil || idx.Snapshot() != nil {
		t.Error("an unpublished index answered something")
	}
}

// The published map is copied, so a caller that keeps its own map and mutates it
// cannot change what the profiler filters against.
func TestPublishingCopies(t *testing.T) {
	var idx ModuleIndex
	mine := map[string][]string{"cid": {"github.com/acme/web"}}
	idx.Publish(mine)
	mine["cid"][0] = "github.com/attacker/everything"
	mine["other"] = []string{"github.com/acme/b"}

	if got := idx.Modules("cid"); !reflect.DeepEqual(got, []string{"github.com/acme/web"}) {
		t.Errorf("modules = %v; the index aliased the caller's slice", got)
	}
	if idx.Size() != 1 {
		t.Errorf("size = %d; the index aliased the caller's map", idx.Size())
	}
}

// Part of the allow-list now comes from binaries, so an entry that cannot be a
// module path must not become a prefix matching half the internet.
func TestOnlyAPlausibleModulePathIsAnAllowListEntry(t *testing.T) {
	cases := map[string]bool{
		"github.com/acme":             true,
		"github.com/acme/web":         true,
		"gitlab.company.com/team/svc": true,
		"example.com/x":               true,
		"github.com/acme/":            true,  // a trailing slash is the same entry
		"github.com":                  false, // a bare host would admit everything on it
		"github.com/":                 false,
		"main":                        false, // kept by the no-domain rule, not by a list
		"runtime":                     false,
		"":                            false,
		"/":                           false,
		"internal/thing":              false, // no domain: not a published module path
	}
	for prefix, want := range cases {
		if got := ValidModulePrefix(prefix); got != want {
			t.Errorf("ValidModulePrefix(%q) = %v, want %v", prefix, got, want)
		}
	}
}

// And the filter drops them rather than carrying them, so one absurd entry
// cannot widen what leaves.
func TestTheFilterDropsAnImplausibleEntry(t *testing.T) {
	f := NewSymbolFilter([]string{"github.com", "github.com/acme/web"}, ThirdPartyDrop)
	var c FilterCounters
	if !f.keep(Frame{Function: "github.com/acme/web/svc.Handle"}, &c) {
		t.Error("the workload's own frame was redacted")
	}
	if f.keep(Frame{Function: "github.com/someone/else.Do"}, &c) {
		t.Error("a bare host was kept as a prefix and admitted an unrelated module")
	}
}
