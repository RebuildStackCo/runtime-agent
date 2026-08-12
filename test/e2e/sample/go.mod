// A standalone module (its own go.mod, so the parent's ./... never builds it)
// whose module path is neither infrastructure nor this agent. The node e2e tests
// deploy it as a "known Go process": the scanner asserts detection by this exact
// module path and a real Go version; the eBPF capture e2e burns CPU in the
// busywork package and asserts the profiler samples it.
module example.com/rebuildstack-e2e/goworkload

go 1.26

// A real third-party dependency called from the hot path so the capture e2e can
// prove the node symbol filter redacts non-allow-listed module frames.
require github.com/cespare/xxhash/v2 v2.3.0
