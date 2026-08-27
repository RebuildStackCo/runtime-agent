// Package busywork burns CPU in a stable function so the capture e2e (ADR 0011)
// can prove both halves of the node's symbol filter at once. Its module path is
// the allow-list entry, so busywork.Grind survives and dominates the shipped
// profile; Grind also calls github.com/cespare/xxhash, which is not on the list
// and must read [filtered] before the profile leaves the node.
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
