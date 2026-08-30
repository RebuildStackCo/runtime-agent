package nodescan

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"syscall"
)

// The pprof-endpoint marker (ADR 0056 §1). Whether a workload can serve
// `/debug/pprof` is answered from the executable the scanner already opens, not
// from a request to the workload.

// pprofMarker is the function name that is present exactly when
// `net/http/pprof` is linked in. Matched whole: "pprof" alone also occurs in a
// recorded source path, so a substring search reports the package on a binary
// that merely came from a directory named after it.
const pprofMarker = "net/http/pprof.Index"

// maxBinaryScanBytes bounds the search. No Go executable approaches it; past it
// the file is not what this scanner assumes, and the answer is absence.
const maxBinaryScanBytes = 512 << 20

// scanChunkBytes is the read size. Consecutive chunks overlap by
// len(pprofMarker)-1 bytes so a marker straddling a boundary is still found.
const scanChunkBytes = 64 << 10

// binaryKey identifies an executable file rather than a process, which is what
// makes the scan cost once per distinct binary on the node instead of once per
// process per pass: replicas of one image share the file, and the file cannot
// change under a running process.
type binaryKey struct{ dev, ino uint64 }

// binaryKeyOf returns the identity of the file behind an already-opened
// executable. The false result means the platform did not supply one, and the
// caller then scans without caching rather than caching wrongly.
func binaryKeyOf(f *os.File) (binaryKey, bool) {
	fi, err := f.Stat()
	if err != nil {
		return binaryKey{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return binaryKey{}, false
	}
	return binaryKey{dev: asUint64(st.Dev), ino: asUint64(st.Ino)}, true
}

// asUint64 widens whichever integer type the platform chose for a Stat_t field.
// Dev is signed on some and unsigned on others, so a plain conversion is
// redundant on one and required on the other; a type parameter is right on both.
// The value is an opaque identifier here, only ever compared against itself.
func asUint64[T int32 | int64 | uint32 | uint64](v T) uint64 {
	return uint64(v) // #nosec G115 -- an identifier, not a quantity
}

// scanForMarker streams r looking for pprofMarker, holding one chunk plus the
// overlap in memory however large the file is.
func scanForMarker(r io.Reader) bool {
	marker := []byte(pprofMarker)
	overlap := len(marker) - 1
	br := bufio.NewReaderSize(r, scanChunkBytes)
	buf := make([]byte, 0, scanChunkBytes+overlap)
	chunk := make([]byte, scanChunkBytes)
	for {
		n, err := br.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if bytes.Contains(buf, marker) {
				return true
			}
			if len(buf) > overlap {
				buf = append(buf[:0], buf[len(buf)-overlap:]...)
			}
		}
		if err != nil {
			return false
		}
	}
}

// markCache remembers the answer per executable file for the life of the
// scanner, and forgets a file no process in the latest pass runs. Bounded by the
// number of distinct binaries running on the node, never by uptime.
type markCache struct {
	answers map[binaryKey]bool
	seen    map[binaryKey]struct{}
}

func newMarkCache() *markCache {
	return &markCache{answers: map[binaryKey]bool{}, seen: map[binaryKey]struct{}{}}
}

// lookup returns the marker answer for the executable at path, scanning it only
// if this file has not been scanned before. An unreadable file answers false:
// absence is what the funnel downstream acts on, and a binary whose bytes could
// not be read is one no endpoint may be claimed for.
func (c *markCache) lookup(path string) bool {
	f, err := os.Open(path) // #nosec G304 -- path is /proc/<pid>/exe of a live process the scanner is already reading
	if err != nil {
		return false
	}
	key, keyed := binaryKeyOf(f)
	if keyed {
		if answer, ok := c.answers[key]; ok {
			_ = f.Close()
			c.seen[key] = struct{}{}
			return answer
		}
	}
	answer := scanForMarker(io.LimitReader(f, maxBinaryScanBytes))
	_ = f.Close()
	if keyed {
		c.answers[key] = answer
		c.seen[key] = struct{}{}
	}
	return answer
}

// startPass begins a new scan pass; endPass drops every file the pass did not
// look at.
func (c *markCache) startPass() { c.seen = make(map[binaryKey]struct{}, len(c.answers)) }

func (c *markCache) endPass() {
	for key := range c.answers {
		if _, ok := c.seen[key]; !ok {
			delete(c.answers, key)
		}
	}
}
