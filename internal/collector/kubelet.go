package collector

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// Bounds on the agent's kubelet reads. Nothing else imposes one: the API
// server proxies whatever the kubelet sends, for as long as it keeps sending,
// and client-go's transport bounds the dial and the TLS handshake but not the
// response (ADR 0045).
const (
	kubeletRequestTimeout = 10 * time.Second

	// The two limits differ because the two reads do: a summary is held whole
	// in memory and up to kubeletFetchConcurrency of them at once, while an
	// exposition is decoded off the wire and costs bandwidth, not heap.
	maxSummaryBytes  = 8 << 20
	maxCadvisorBytes = 64 << 20

	// kubeletFetchConcurrency is how many nodes are read at once. It is what
	// makes one unresponsive kubelet cost one node's timeout instead of the
	// whole cycle's (ADR 0045).
	kubeletFetchConcurrency = 8
)

var errResponseTooLarge = errors.New("kubelet response is larger than the read limit")

// cappedReader fails once the stream passes limit bytes rather than reporting
// the end of it. A truncated response is not a smaller response: a short
// exposition decodes as a node with fewer containers, which is exactly what a
// quiet node looks like (ADR 0045).
type cappedReader struct {
	r     io.Reader
	limit int64
	read  int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if remaining := c.limit - c.read; int64(len(p)) > remaining+1 {
		p = p[:remaining+1]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.limit {
		return n, fmt.Errorf("%w of %d bytes", errResponseTooLarge, c.limit)
	}
	return n, err
}

func readCapped(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(&cappedReader{r: r, limit: limit})
}
