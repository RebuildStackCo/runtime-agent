package main

// The set of payload writers that run on the agent's own cadence, as data.
//
// There are two passes over it — the periodic one and the one a shutdown makes
// — and they must agree about which writers exist and in what order. As two
// hand-written call sequences they did not: the metadata flush was in the
// periodic pass and absent from the shutdown pass, correctly, but nothing said
// so and nothing kept the two lists from drifting further.

// flusher is one payload writer. Its identifier at the call site is its label;
// the struct carries only what the two passes have to decide between them.
type flusher struct {
	run func()
	// onShutdown is whether this writer also runs on the pass a shutdown makes.
	//
	// False for a writer that re-derives its payload from the watchers' live
	// indexes: by shutdown those have stopped, so the pass would republish the
	// previous state under a fresh capture instant. The writers that accumulate
	// — the journals, the inventory — are the opposite case, and are exactly
	// what the shutdown pass exists for: without it a graceful stop drops up to
	// one coverage interval of them.
	onShutdown bool
}

// runFlushers runs the set in order. On the shutdown pass it skips the writers
// that declare themselves periodic-only.
func runFlushers(flushers []flusher, shutdown bool) {
	for _, f := range flushers {
		if shutdown && !f.onShutdown {
			continue
		}
		f.run()
	}
}
