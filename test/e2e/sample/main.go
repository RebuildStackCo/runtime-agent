// Command goworkload is a long-running Go process used as the known target of
// the node e2e tests: a stable non-infrastructure module path for the scanner,
// CPU burnt in a symbolizable allow-listed function for the eBPF capture (see
// the busywork package), and — for ADR 0056 — a linked net/http/pprof and one
// bound port of each reachability class.
package main

import (
	"log"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof on DefaultServeMux

	"example.com/rebuildstack-e2e/goworkload/busywork"
)

func main() {
	log.Println("e2e goworkload started")
	// Reachable from outside the pod: what a real service exposing pprof looks
	// like. Bound before the CPU burn starts so the socket exists by the first
	// node scan.
	serve("0.0.0.0:6060")
	// Reachable only from inside the pod, which is the other class the reader
	// has to report — and the one a puller must not be sent after.
	serve("127.0.0.1:9090")
	// Never returns: burns CPU in an allow-listed own-module function that also
	// calls a third-party dependency, so the capture e2e can prove both the keep
	// and the redact paths of the node symbol filter.
	busywork.Grind()
}

func serve(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listening on %s: %v", addr, err)
	}
	go func() { log.Println(http.Serve(ln, nil)) }() //nolint:gosec // no timeouts: an e2e fixture, not a served workload
}
