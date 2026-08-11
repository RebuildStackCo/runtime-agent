package nodeprofile

import (
	"errors"
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
