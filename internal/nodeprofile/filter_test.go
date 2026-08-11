package nodeprofile

import (
	"strings"
	"testing"
)

func goFrame(fn string) Frame     { return Frame{Function: fn, Kind: "go"} }
func kernelFrame(fn string) Frame { return Frame{Function: fn, Kind: "kernel"} }

func fnsOf(frames []Frame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.Function
	}
	return out
}

func TestPackagePath(t *testing.T) {
	cases := map[string]string{
		"github.com/acme/app/svc.(*S).Do":        "github.com/acme/app/svc",
		"runtime.mallocgc":                       "runtime",
		"net/http.(*conn).serve":                 "net/http",
		"main.process":                           "main",
		"entry_SYSCALL_64":                       "entry_SYSCALL_64",
		"golang.org/x/sync/errgroup.(*Group).Go": "golang.org/x/sync/errgroup",
	}
	for in, want := range cases {
		if got := packagePath(in); got != want {
			t.Errorf("packagePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasDomain(t *testing.T) {
	for pkg, want := range map[string]bool{
		"github.com/acme/app": true,
		"golang.org/x/sync":   true,
		"runtime":             false,
		"net/http":            false,
		"main":                false,
		"internal/abi":        false,
		"entry_SYSCALL_64":    false,
	} {
		if got := hasDomain(pkg); got != want {
			t.Errorf("hasDomain(%q) = %v, want %v", pkg, got, want)
		}
	}
}

func TestSymbolFilterClassifiesAndRedacts(t *testing.T) {
	f := NewSymbolFilter([]string{"github.com/acme/app"}, ThirdPartyDrop)
	in := []Sample{{
		Value: 5,
		Frames: []Frame{
			goFrame("github.com/acme/app/svc.(*S).Do"),        // client -> keep
			goFrame("github.com/acme/app/internal/util.Mix"),  // client -> keep
			goFrame("github.com/thirdparty/lib/foo.Bar"),      // third-party -> redact
			goFrame("golang.org/x/sync/errgroup.(*Group).Go"), // third-party -> redact (collapses)
			goFrame("runtime.mallocgc"),                       // stdlib -> keep
			goFrame("main.process"),                           // workload main -> keep
			kernelFrame("entry_SYSCALL_64"),                   // kernel -> keep
		},
	}}

	out, c := f.Filter(in)
	got := fnsOf(out[0].Frames)
	want := []string{
		"github.com/acme/app/svc.(*S).Do",
		"github.com/acme/app/internal/util.Mix",
		RedactedFrame, // the two consecutive third-party frames collapse to one
		"runtime.mallocgc",
		"main.process",
		"entry_SYSCALL_64",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("frames = %v\n          want %v", got, want)
	}
	if c.ThirdPartyDropped != 2 || c.SamplesFiltered != 1 {
		t.Errorf("counters = %+v, want ThirdPartyDropped=2 SamplesFiltered=1", c)
	}
	for _, fn := range got {
		if strings.Contains(fn, "thirdparty") || strings.Contains(fn, "golang.org") {
			t.Errorf("redacted identity leaked into output: %q", fn)
		}
	}
}

func TestSymbolFilterThirdPartyKeep(t *testing.T) {
	f := NewSymbolFilter([]string{"github.com/acme/app"}, ThirdPartyKeep)
	out, c := f.Filter([]Sample{{Frames: []Frame{goFrame("github.com/thirdparty/lib.Bar")}}})
	if out[0].Frames[0].Function != "github.com/thirdparty/lib.Bar" || c.ThirdPartyDropped != 0 {
		t.Errorf("ThirdPartyKeep should keep third-party: %v, counters %+v", fnsOf(out[0].Frames), c)
	}
}

func TestSymbolFilterUnsymbolizedRedacted(t *testing.T) {
	f := NewSymbolFilter(nil, ThirdPartyDrop)
	out, c := f.Filter([]Sample{{Frames: []Frame{
		{Function: "", Kind: "native"},
		{Function: "", Kind: "native"},
		goFrame("main.run"),
	}}})
	got := fnsOf(out[0].Frames)
	if strings.Join(got, "|") != RedactedFrame+"|main.run" {
		t.Errorf("frames = %v, want [%s main.run]", got, RedactedFrame)
	}
	if c.UnsymbolizedDropped != 2 {
		t.Errorf("UnsymbolizedDropped = %d, want 2", c.UnsymbolizedDropped)
	}
}

// TestSymbolFilterEmptyAllowListStillKeepsStdlibAndMain guards the deny-by-default
// posture: with no allow-list, client third-party is gone but stdlib/main/kernel
// remain so the profile is still readable.
func TestSymbolFilterEmptyAllowListStillKeepsStdlibAndMain(t *testing.T) {
	f := NewSymbolFilter(nil, ThirdPartyDrop)
	out, _ := f.Filter([]Sample{{Frames: []Frame{
		goFrame("main.process"),
		goFrame("runtime.mallocgc"),
		goFrame("github.com/acme/app/svc.Do"), // no allow-list -> third-party -> redact
	}}})
	got := fnsOf(out[0].Frames)
	want := []string{"main.process", "runtime.mallocgc", RedactedFrame}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("frames = %v, want %v", got, want)
	}
}
