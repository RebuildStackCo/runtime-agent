// Package nodeprofile drives the embedded eBPF CPU profiler
// (go.opentelemetry.io/ebpf-profiler) on the node and accumulates the
// symbolized samples it returns (ADR 0011). Symbolization happens on the node,
// inside the profiler; this package never copies or uploads a binary.
//
// Invariant (ADR 0011 §4, constraint a): before the allow-list filter runs
// (a later slice), a captured profile exists nowhere but the in-memory Buffer
// here. Nothing in this package logs or ships a frame's function name or file —
// only aggregate counts. The filter and pprof serialization that let symbols
// leave the node arrive in later slices and operate on the decoupled Frame /
// Sample types below, not on the upstream profiler types.
package nodeprofile

import "sync"

// Frame is one symbolized stack frame, decoupled from the upstream profiler
// types so the filter and serializer stay testable and stable. Function and
// File are the symbol identity that MUST NOT leave the node before filtering.
//
// File is a base name — "server.go", never a directory (ADR 0041). The
// reporter cuts it at the seam, so the compiler-recorded path, which on a
// binary built without -trimpath is the build machine's absolute path, is never
// held here at all.
type Frame struct {
	Function string
	File     string
	Line     int64
	Kind     string // upstream FrameType (native, kernel, interpreted, ...)
}

// Sample is one captured stack with its weight and the identity of the process
// it came from. PID/Comm/ContainerID are for keying profiles per workload in a
// later slice; environment variables from the profiler are deliberately dropped
// (they routinely carry secrets — CLAUDE.md invariant 4).
type Sample struct {
	Frames      []Frame
	Value       int64
	PID         int64
	Comm        string
	ContainerID string
}

// Buffer accumulates captured samples in process memory. It is the *only* place
// a pre-filter profile exists. Counters returns aggregates that are safe to log;
// Samples hands the raw stacks to the filter/serializer in later slices.
type Buffer struct {
	mu      sync.Mutex
	samples []Sample
	frames  uint64
}

// Add records a captured sample.
func (b *Buffer) Add(s Sample) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.samples = append(b.samples, s)
	b.frames += uint64(len(s.Frames))
}

// Counters returns aggregate counts only — never identities. Safe to log.
func (b *Buffer) Counters() (samples, frames uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return uint64(len(b.samples)), b.frames
}

// Samples returns a copy of the accumulated samples for downstream filtering
// and serialization (later slices).
func (b *Buffer) Samples() []Sample {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Sample, len(b.samples))
	copy(out, b.samples)
	return out
}

// Drain returns the accumulated samples and resets the buffer so the next
// capture window starts empty. It is how the pipeline cuts a window.
func (b *Buffer) Drain() []Sample {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.samples
	b.samples = nil
	b.frames = 0
	return out
}
