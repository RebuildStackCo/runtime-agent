// Package busywork burns CPU in a stable, allow-listed function so the eBPF
// capture e2e (ADR 0011) can prove two things about the node's on-node symbol
// filter at once:
//
//  1. Keep: this package's module path (example.com/rebuildstack-e2e/goworkload)
//     is the customer allow-list entry, so busywork.Grind survives filtering and
//     is the dominant service function in the shipped profile.
//  2. Redact: Grind also calls a real third-party dependency
//     (github.com/cespare/xxhash), whose frames are NOT on the allow-list and so
//     must be replaced by the [filtered] placeholder before the profile leaves
//     the node.
package busywork

import "github.com/cespare/xxhash/v2"

// Grind runs forever. Each outer iteration does a large amount of the workload's
// own arithmetic — this is the leaf where most CPU samples land, so busywork.Grind
// ranks top by cumulative value — and then hashes the buffer with the third-party
// module, whose body (a real loop, not inlined into Grind) shows up in a fraction
// of the samples and must be redacted on the node. It never returns.
func Grind() {
	buf := make([]byte, 4096)
	var acc uint64
	for {
		// Own-module arithmetic. The write into buf and the feedback through acc
		// keep the loop live (not optimized away) and keep this the hot leaf.
		for i := range 4_000_000 {
			acc = acc*1103515245 + 12345
			buf[i%len(buf)] = byte(acc >> 33)
		}
		// Third-party call: its frames are not allow-listed and must become
		// [filtered]. XORing the result back into acc keeps it observable.
		acc ^= xxhash.Sum64(buf)
	}
}
