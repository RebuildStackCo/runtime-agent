// Command goworkload is a minimal, long-running Go process used as the known
// target of the node-scanner e2e test. Its only job is to exist with a stable,
// non-infrastructure module path so the scanner can find it via buildinfo.
package main

import (
	"log"
	"time"
)

func main() {
	log.Println("e2e goworkload started")
	for {
		time.Sleep(time.Hour)
	}
}
