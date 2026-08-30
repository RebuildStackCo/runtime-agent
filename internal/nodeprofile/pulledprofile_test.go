package nodeprofile

import (
	"bytes"
	"errors"
	"fmt"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
)

// burnInADependency spends CPU inside a third-party module, so a real profile of
// this test has frames the allow-list must redact. Merging profiles is pure
// pprof-library work, with no compression or syscall to hide in.
func burnInADependency(until time.Time) {
	src := syntheticProfile(2000)
	for time.Now().Before(until) {
		if _, err := profile.Merge([]*profile.Profile{src.Copy(), src.Copy()}); err != nil {
			return
		}
	}
}

// syntheticProfile builds a profile large enough that merging it costs real
// time. Its own frame names are irrelevant — what is being profiled is the
// library doing the merging.
func syntheticProfile(samples int) *profile.Profile {
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		PeriodType: &profile.ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:     1,
	}
	for i := range samples {
		fn := &profile.Function{ID: uint64(i + 1), Name: fmt.Sprintf("synthetic.f%d", i)}
		loc := &profile.Location{ID: uint64(i + 1), Line: []profile.Line{{Function: fn}}}
		p.Function = append(p.Function, fn)
		p.Location = append(p.Location, loc)
		p.Sample = append(p.Sample, &profile.Sample{Location: []*profile.Location{loc}, Value: []int64{1}})
	}
	return p
}

// realCPUProfile captures a genuine profile of this process, which is what a
// pulled profile is: bytes another Go runtime assembled, not samples this agent
// collected.
func realCPUProfile(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := pprof.StartCPUProfile(&buf); err != nil {
		t.Skipf("cannot start a CPU profile here: %v", err)
	}
	burnInADependency(time.Now().Add(300 * time.Millisecond))
	pprof.StopCPUProfile()
	if buf.Len() == 0 {
		t.Skip("the runtime produced no profile")
	}
	return buf.Bytes()
}

// The load-bearing assertion of the whole pull path: after filtering, nothing in
// the encoded bytes names a package outside the allow-list. Not the function
// table, not a file path, not a mapping, not a label.
func TestAPulledProfileCarriesNoIdentityOutsideTheAllowList(t *testing.T) {
	raw := realCPUProfile(t)
	f := NewSymbolFilter([]string{"github.com/RebuildStackCo/runtime-agent"}, ThirdPartyDrop)

	out, counters, err := f.FilterPulled(raw)
	if err != nil {
		t.Fatalf("filtering a real profile failed: %v", err)
	}
	if counters.ThirdPartyDropped == 0 {
		t.Error("nothing was redacted from a profile whose hot path is a third-party module")
	}

	p, err := profile.ParseData(out)
	if err != nil {
		t.Fatalf("the filtered profile does not parse: %v", err)
	}
	for _, fn := range p.Function {
		if fn.Name == RedactedFrame {
			continue
		}
		if pkg := packagePath(fn.Name); hasDomain(pkg) && !f.isAllowed(pkg) {
			t.Errorf("function %q survived the filter", fn.Name)
		}
		if strings.Contains(fn.Filename, "/") {
			t.Errorf("filename %q is a path; only a base name may leave (ADR 0041)", fn.Filename)
		}
	}
	if len(p.Mapping) != 0 {
		t.Errorf("mappings survived: %+v — they name the executable and its build ID", p.Mapping)
	}
	for _, s := range p.Sample {
		if len(s.Label) != 0 || len(s.NumLabel) != 0 {
			t.Errorf("sample labels survived: %v %v — they are strings the profiled service chose", s.Label, s.NumLabel)
		}
	}
}

// The other direction: a profile filtered with the customer's own module allowed
// must still be worth something.
func TestOwnCodeAndTheStandardLibrarySurvive(t *testing.T) {
	raw := realCPUProfile(t)
	f := NewSymbolFilter([]string{"github.com/RebuildStackCo/runtime-agent"}, ThirdPartyDrop)

	out, _, err := f.FilterPulled(raw)
	if err != nil {
		t.Fatalf("filtering failed: %v", err)
	}
	p, err := profile.ParseData(out)
	if err != nil {
		t.Fatalf("the filtered profile does not parse: %v", err)
	}
	var own, stdlib int
	for _, fn := range p.Function {
		switch {
		case strings.HasPrefix(fn.Name, "github.com/RebuildStackCo/runtime-agent"):
			own++
		case !hasDomain(packagePath(fn.Name)) && fn.Name != RedactedFrame:
			stdlib++
		}
	}
	if own == 0 {
		t.Error("no frame of the module under test survived; the filter kept nothing useful")
	}
	if stdlib == 0 {
		t.Error("no standard-library frame survived; a profile of only own code is not a flame graph")
	}
}

// A profile that does not parse must produce nothing, not an empty profile that
// looks like a quiet workload.
func TestUnparseableBytesAreAnErrorNotAnEmptyProfile(t *testing.T) {
	f := NewSymbolFilter(nil, ThirdPartyDrop)
	out, _, err := f.FilterPulled([]byte("this is not a profile"))
	if !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("error = %v, want ErrInvalidProfile", err)
	}
	if out != nil {
		t.Errorf("bytes = %q, want none", out)
	}
}

// Keeping third-party frames is a configuration, and it must actually keep them:
// the option exists because the finding it enables — where CPU goes by module —
// is unusable without it.
func TestKeepingThirdPartyFramesKeepsThem(t *testing.T) {
	raw := realCPUProfile(t)
	f := NewSymbolFilter([]string{"github.com/RebuildStackCo/runtime-agent"}, ThirdPartyKeep)

	out, counters, err := f.FilterPulled(raw)
	if err != nil {
		t.Fatalf("filtering failed: %v", err)
	}
	if counters.ThirdPartyDropped != 0 {
		t.Errorf("third-party frames dropped = %d under the keep policy", counters.ThirdPartyDropped)
	}
	p, err := profile.ParseData(out)
	if err != nil {
		t.Fatalf("the filtered profile does not parse: %v", err)
	}
	found := false
	for _, fn := range p.Function {
		if strings.HasPrefix(fn.Name, "github.com/google/pprof") {
			found = true
		}
	}
	if !found {
		t.Error("no third-party frame survived under the keep policy")
	}
}
