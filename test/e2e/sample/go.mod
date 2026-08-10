// A standalone module (its own go.mod, so the parent's ./... never builds it)
// whose module path is neither infrastructure nor this agent. The node-scanner
// e2e deploys it as a "known Go process" and asserts the scanner detects it
// with this exact module path and a real Go version.
module example.com/rebuildstack-e2e/goworkload

go 1.26
