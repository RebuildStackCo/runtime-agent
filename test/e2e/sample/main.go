// Command goworkload is a long-running Go process used as the known target of
// the node e2e tests. For the scanner e2e it only needs to exist with a stable,
// non-infrastructure module path (detected via buildinfo). For the eBPF capture
// e2e it must also burn CPU in a symbolizable, allow-listed function so the
// profiler has something to sample — see the busywork package.
package main

import (
	"log"

	"example.com/rebuildstack-e2e/goworkload/busywork"
)

func main() {
	log.Println("e2e goworkload started")
	// Never returns: burns CPU in an allow-listed own-module function that also
	// calls a third-party dependency, so the capture e2e can prove both the keep
	// and the redact paths of the node symbol filter.
	busywork.Grind()
}
