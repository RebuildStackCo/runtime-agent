package nodeprofile

import (
	"bytes"
	"fmt"
	"path/filepath"

	"github.com/google/pprof/profile"
)

// Filtering a profile the agent did not build (ADR 0058). The eBPF path hands
// this package symbolized samples and gets bytes back; a pulled profile arrives
// as bytes already assembled by someone else's runtime, and everything in it is
// a string that runtime chose.
//
// So this is not the same operation as Filter with a different input type: it
// also has to remove the fields a Go runtime puts in a profile and this agent
// has never collected. The allow-list decision itself is `keep`, shared, once.

// maxPulledFrames bounds a stack. A Go profile truncates at 64 frames of its
// own accord; past this the bytes are not what this parser assumes.
const maxPulledFrames = 256

// FilterPulled parses a gzipped pprof profile, redacts every frame the
// allow-list does not admit, strips the fields listed in FilterPulled's own
// contract below, and returns the re-encoded bytes with the aggregate counts.
//
// The input bytes are never returned in any form: what comes back is built from
// what survived. An unparseable profile is an error, not an empty result — the
// caller counts it and ships nothing.
func (f *SymbolFilter) FilterPulled(gzpprof []byte) ([]byte, FilterCounters, error) {
	p, err := profile.ParseData(gzpprof)
	if err != nil {
		return nil, FilterCounters{}, fmt.Errorf("%w: parse: %w", ErrInvalidProfile, err)
	}
	counters := f.redactProfile(p)
	stripNonSymbolIdentity(p)
	if err := p.CheckValid(); err != nil {
		return nil, counters, fmt.Errorf("%w: %w", ErrInvalidProfile, err)
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		return nil, counters, fmt.Errorf("%w: encode: %w", ErrInvalidProfile, err)
	}
	return buf.Bytes(), counters, nil
}

// redactProfile replaces disallowed frames with the placeholder and collapses
// consecutive redacted locations, the same reduction filterFrames performs on
// captured samples: the shape of the stack survives, no identity does.
func (f *SymbolFilter) redactProfile(p *profile.Profile) FilterCounters {
	var c FilterCounters

	placeholder := &profile.Function{ID: 1, Name: RedactedFrame}
	kept := map[*profile.Function]bool{}
	// A location is fully redacted when nothing in it survives; its lines are
	// the inlined frames the compiler recorded at that address.
	redactedWhole := map[*profile.Location]bool{}

	for _, loc := range p.Location {
		lines := make([]profile.Line, 0, len(loc.Line))
		prevRedacted := false
		for _, line := range loc.Line {
			if line.Function != nil && f.keep(Frame{Function: line.Function.Name}, &c) {
				kept[line.Function] = true
				lines = append(lines, line)
				prevRedacted = false
				continue
			}
			if line.Function == nil {
				c.UnsymbolizedDropped++
			}
			if prevRedacted {
				continue
			}
			lines = append(lines, profile.Line{Function: placeholder})
			prevRedacted = true
		}
		if len(lines) == 1 && lines[0].Function == placeholder {
			redactedWhole[loc] = true
		}
		loc.Line = lines
		loc.IsFolded = false
	}

	for _, s := range p.Sample {
		locs := make([]*profile.Location, 0, min(len(s.Location), maxPulledFrames))
		prevRedacted := false
		redactedAny := false
		for i, loc := range s.Location {
			if i >= maxPulledFrames {
				break
			}
			if !redactedWhole[loc] {
				locs = append(locs, loc)
				prevRedacted = false
				continue
			}
			redactedAny = true
			if prevRedacted {
				continue
			}
			locs = append(locs, loc)
			prevRedacted = true
		}
		if redactedAny {
			c.SamplesFiltered++
		}
		s.Location = locs
	}

	// The function table is rebuilt rather than left alone: an entry no sample
	// references any more still carries its name in the encoded bytes, so
	// leaving the table would ship every redacted identity beside a profile
	// that does not use it.
	funcs := make([]*profile.Function, 0, len(kept)+1)
	funcs = append(funcs, placeholder)
	nextID := uint64(2)
	for _, fn := range p.Function {
		if !kept[fn] {
			continue
		}
		fn.ID = nextID
		nextID++
		fn.Filename = filepath.Base(fn.Filename)
		funcs = append(funcs, fn)
	}
	p.Function = funcs
	return c
}

// stripNonSymbolIdentity removes what a Go runtime writes into a profile and
// this agent collects from nowhere else: the mappings, which name the
// executable's path on disk and its build ID; the sample labels, which are
// arbitrary strings the profiled service chose and may hold anything; and the
// free-text comments.
func stripNonSymbolIdentity(p *profile.Profile) {
	p.Mapping = nil
	for _, loc := range p.Location {
		loc.Mapping = nil
		loc.Address = 0
	}
	for _, s := range p.Sample {
		s.Label = nil
		s.NumLabel = nil
		s.NumUnit = nil
	}
	p.Comments = nil
	p.DefaultSampleType = ""
	p.DropFrames = ""
	p.KeepFrames = ""
}
