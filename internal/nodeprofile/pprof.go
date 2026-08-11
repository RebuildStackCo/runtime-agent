package nodeprofile

import (
	"bytes"

	"github.com/google/pprof/profile"
)

// Serialize turns filtered samples into a gzipped pprof profile with a
// cpu/nanoseconds sample type. The eBPF profiler reports sample counts; we
// convert to CPU nanoseconds using the sampling period (1e9/rate ns per sample)
// so the result matches the cpu/nanoseconds contract the profile is validated
// and documented against (ADR 0011 §5, security.md §8). The function and
// location tables are built in first-seen order for a deterministic encoding.
//
// Input samples are expected to be already allow-list-filtered (slice 4);
// serialization does not itself redact anything.
func Serialize(samples []Sample, samplesPerSecond int) ([]byte, error) {
	if samplesPerSecond <= 0 {
		samplesPerSecond = 20
	}
	periodNs := int64(1_000_000_000) / int64(samplesPerSecond)

	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		PeriodType: &profile.ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:     periodNs,
	}

	type funcKey struct{ name, file string }
	funcs := map[funcKey]*profile.Function{}
	locs := map[Frame]*profile.Location{}
	var funcID, locID uint64

	locationFor := func(fr Frame) *profile.Location {
		if l, ok := locs[fr]; ok {
			return l
		}
		fk := funcKey{fr.Function, fr.File}
		fn, ok := funcs[fk]
		if !ok {
			funcID++
			fn = &profile.Function{ID: funcID, Name: fr.Function, Filename: fr.File}
			funcs[fk] = fn
			p.Function = append(p.Function, fn)
		}
		locID++
		l := &profile.Location{ID: locID, Line: []profile.Line{{Function: fn, Line: fr.Line}}}
		locs[fr] = l
		p.Location = append(p.Location, l)
		return l
	}

	for _, s := range samples {
		locations := make([]*profile.Location, 0, len(s.Frames))
		for _, fr := range s.Frames {
			locations = append(locations, locationFor(fr))
		}
		value := s.Value * periodNs
		if value <= 0 {
			value = periodNs
		}
		p.Sample = append(p.Sample, &profile.Sample{
			Location: locations,
			Value:    []int64{value},
		})
	}

	if err := p.CheckValid(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
