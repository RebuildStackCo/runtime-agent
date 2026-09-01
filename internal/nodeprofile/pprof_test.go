package nodeprofile

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/pprof/profile"
)

func TestSerializeProducesCPUNanosProfile(t *testing.T) {
	const rate = 20
	period := int64(1_000_000_000) / rate

	data, err := Serialize([]Sample{
		{Value: 3, Frames: []Frame{goFrame("main.hot"), goFrame("runtime.mallocgc")}},
		{Value: 1, Frames: []Frame{goFrame("main.hot")}},
	}, rate)
	if err != nil {
		t.Fatal(err)
	}

	p, err := profile.ParseData(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.SampleType) != 1 || p.SampleType[0].Type != "cpu" || p.SampleType[0].Unit != "nanoseconds" {
		t.Fatalf("sample type = %+v, want cpu/nanoseconds", p.SampleType)
	}
	if got, want := p.Sample[0].Value[0], 3*period; got != want {
		t.Errorf("sample value = %d, want count×period = %d", got, want)
	}
	names := map[string]bool{}
	for _, f := range p.Function {
		names[f.Name] = true
	}
	if !names["main.hot"] || !names["runtime.mallocgc"] {
		t.Errorf("functions missing from profile: %v", names)
	}
}

func TestValidateAcceptsGoodProfile(t *testing.T) {
	data, err := Serialize([]Sample{
		{Value: 5, Frames: []Frame{goFrame("main.process"), goFrame("runtime.mallocgc")}},
	}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(data); err != nil {
		t.Errorf("Validate rejected a good profile: %v", err)
	}
}

func TestValidateRejectsBadProfiles(t *testing.T) {
	onlyRuntime, _ := Serialize([]Sample{
		{Value: 5, Frames: []Frame{goFrame("runtime.mallocgc"), goFrame("runtime.gcBgMarkWorker")}},
	}, 20)
	onlyFiltered, _ := Serialize([]Sample{
		{Value: 5, Frames: []Frame{{Function: RedactedFrame, Kind: "filtered"}}},
	}, 20)

	cases := map[string][]byte{
		"only runtime.*":  onlyRuntime,
		"only [filtered]": onlyFiltered,
		"not a pprof":     []byte("garbage not a profile"),
	}
	for name, data := range cases {
		if err := Validate(data); err == nil || !errors.Is(err, ErrInvalidProfile) {
			t.Errorf("%s: expected ErrInvalidProfile, got %v", name, err)
		}
	}
}

// TestNoShippedFunctionCarriesADirectory asserts the property on the bytes that
// actually leave the node, not on the struct that produced them (ADR 0041).
//
// The reporter is what cuts the path, and the reporter is one package away from
// the serializer. Asserting only there would let a later change reintroduce a
// full path by any other route — a second producer of Frames, a field copied
// somewhere new — and this test would still be the thing that catches it,
// because it reads the shipped profile.
func TestNoShippedFunctionCarriesADirectory(t *testing.T) {
	// Frames as they exist after the reporter: base names only.
	data, err := Serialize([]Sample{{
		Value: 4,
		Frames: []Frame{
			{Function: "main.(*Server).handleCharge", File: "server.go", Line: 412, Kind: "native"},
			{Function: "github.com/acme/app/billing.Apply", File: "ledger.go", Line: 88, Kind: "native"},
			{Function: "runtime.mallocgc", File: "malloc.go", Line: 1000, Kind: "native"},
		},
	}}, 20)
	if err != nil {
		t.Fatal(err)
	}

	p, err := profile.ParseData(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Function) == 0 {
		t.Fatal("the profile carries no functions; the assertion below would be vacuous")
	}
	for _, fn := range p.Function {
		if strings.ContainsAny(fn.Filename, `/\`) {
			t.Errorf("function %q ships filename %q, which names a directory", fn.Name, fn.Filename)
		}
	}
}

// TestFilesOfTheSameNameInDifferentPackagesStayDistinct pins what makes the base
// name sufficient (ADR 0041): a symbolized Go function name carries its package
// path, and a Go package is a directory, so two files of the same name cannot
// share one. Package path plus base name is what `go build -trimpath` records.
//
// It rests on the serializer keying a function on its name, not its file alone.
// Narrow the key to the file and these three frames collapse into one entry —
// which is the case this test exists to catch.
func TestFilesOfTheSameNameInDifferentPackagesStayDistinct(t *testing.T) {
	data, err := Serialize([]Sample{{Value: 4, Frames: []Frame{
		{Function: "github.com/acme/app/api.(*Server).Serve", File: "server.go", Line: 41, Kind: "native"},
		{Function: "github.com/acme/app/grpc.(*Server).Serve", File: "server.go", Line: 77, Kind: "native"},
		{Function: "github.com/acme/app/internal/admin.(*Server).Serve", File: "server.go", Line: 12, Kind: "native"},
	}}}, 20)
	if err != nil {
		t.Fatal(err)
	}
	p, err := profile.ParseData(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Function) != 3 {
		t.Fatalf("three same-named files in three packages produced %d functions, want 3", len(p.Function))
	}
	seen := map[string]bool{}
	for _, fn := range p.Function {
		if seen[fn.Name] {
			t.Errorf("function %q appears twice", fn.Name)
		}
		seen[fn.Name] = true
	}
}

// gcHeavyProfile is a service spending most of its CPU in the collector: five
// runtime frames outrank its own code, which is what a GC problem looks like in
// a profile. Background marking runs on its own goroutines, so those samples
// carry no service frame at all — the service function can only rank below them.
func gcHeavyProfile(t *testing.T) []byte {
	t.Helper()
	gc := []Frame{
		goFrame("runtime.gcBgMarkWorker"), goFrame("runtime.gcDrain"),
		goFrame("runtime.scanobject"), goFrame("runtime.markroot"),
		goFrame("runtime.systemstack"),
	}
	data, err := Serialize([]Sample{
		{Value: 70, Frames: gc},
		{Value: 20, Frames: []Frame{goFrame("main.serve"), goFrame("runtime.mallocgc")}},
	}, 20)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The profile that proves a GC problem is the one the validator used to refuse:
// ranking by cumulative value puts five runtime frames on top, and the service's
// own function below them. Refusing it turned the strongest evidence of a
// collector problem into silence (ADR 0063).
func TestAProfileDominatedByTheCollectorStillShips(t *testing.T) {
	if err := Validate(gcHeavyProfile(t)); err != nil {
		t.Errorf("a GC-dominated profile was refused: %v", err)
	}
}

// And the case the validator exists for still fails: a profile with no service
// function anywhere is one whose service layer the filter ate, and it says
// nothing about the workload.
func TestAProfileWithNoServiceFunctionAtAllIsStillRefused(t *testing.T) {
	noService, _ := Serialize([]Sample{
		{Value: 70, Frames: []Frame{goFrame("runtime.gcBgMarkWorker"), goFrame("runtime.gcDrain")}},
		{Value: 20, Frames: []Frame{{Function: RedactedFrame, Kind: "filtered"}, goFrame("runtime.mallocgc")}},
	}, 20)
	if err := Validate(noService); err == nil || !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("expected ErrInvalidProfile for a profile with no service frame, got %v", err)
	}
}
