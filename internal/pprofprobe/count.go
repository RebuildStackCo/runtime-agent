package pprofprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

// Reading the goroutine count off the page this package already fetches
// (ADR 0080). The index renders one row per profile with the profile's own
// count, and for `goroutine` that count is `runtime.NumGoroutine()`: a variable
// read, no profiler, no stop-the-world, nothing allocated in the workload.
//
// So this is the prober's request repeated, not a new kind of access — which is
// why it lives beside it rather than in a package of its own. The path constant
// stays the one thing this package owns.

const (
	// countTimeout bounds one reading. The page is rendered from memory and the
	// count is a variable read, so a slow answer is a sick workload rather than
	// a busy one — the same reasoning as probeTimeout, and shorter because this
	// request repeats where the probe happens once.
	countTimeout = 2 * time.Second
	// maxReadingsPerRound bounds one round. Sequential like the probe round, for
	// the same reason: the cost of being slow is a later sample, and the cost of
	// being parallel is a burst of connections into the customer's workloads.
	maxReadingsPerRound = 200
	// heldAfterFailure is how long a target that answered badly, or not at all,
	// is left out of the round. Without it a few dead endpoints at the front of
	// a deterministic order spend the round's whole budget and the tail is never
	// reached (ADR 0080 §2). Five minutes costs a broken endpoint one reading in
	// five rather than every one, and still samples a pod that came back inside
	// the same window.
	heldAfterFailure = 5 * time.Minute
)

// CountSink takes one reading. Returning nothing: a sample that cannot be
// accumulated is lost, which is the loss-harmless posture of everything else
// the agent holds in memory (ADR 0003).
type CountSink func(model.GoroutineCount)

// Replica resolves a candidate to one pod to read from: the address to connect
// to, and that pod's name.
//
// The address is a connection parameter and enters no record and no payload,
// exactly as Address promises (ADR 0057 §3). The name does travel, and it is
// already a collected fact — the restart and disruption journals carry it.
type Replica func(Candidate) (addr, pod string, ok bool)

// CountCoverage is what the counter did, for the collection-coverage payload.
// Counts only, never which workload could not be read (ADR 0054).
type CountCoverage struct {
	// Sampled is readings taken and handed to the sink.
	Sampled int `json:"sampled"`
	// Unreachable is targets whose connection failed or which had no addressable
	// replica, and Unreadable is answers that arrived and were not the pprof
	// index, or were and carried no goroutine row.
	Unreachable int `json:"unreachable"`
	Unreadable  int `json:"unreadable"`
}

// Counter reads the goroutine count of every confirmed endpoint, every round.
//
// Every confirmed endpoint rather than a rotation: the reading is worth nothing
// alone and everything as a series, so a workload sampled half the time
// produces a series with holes where the interesting part might be (ADR 0080 §2).
type Counter struct {
	client  *http.Client
	replica Replica
	sink    CountSink
	logger  *slog.Logger
	now     func() time.Time

	mu    sync.Mutex
	cover CountCoverage
	// held is when each target may be read again, for the targets that failed.
	held map[Target]time.Time
}

// NewCounter builds a counter. The client is this package's own, on the same
// terms as the prober's: no proxy from the environment, no redirect, no reuse.
func NewCounter(replica Replica, sink CountSink, logger *slog.Logger) *Counter {
	return &Counter{
		client:  newClient(countTimeout),
		replica: replica,
		sink:    sink,
		logger:  logger,
		now:     time.Now,
		held:    map[Target]time.Time{},
	}
}

// Run reads on every tick until ctx ends. confirmed supplies the candidates
// whose endpoint the prober has confirmed; the counter never decides what is
// readable, only what to read next.
func (c *Counter) Run(ctx context.Context, interval time.Duration, confirmed func() []Candidate) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A round may not outlive its own interval: rounds that overlap
			// would double the connections a workload sees and would sample one
			// instant twice.
			round, cancel := context.WithTimeout(ctx, interval)
			c.round(round, confirmed())
			cancel()
		}
	}
}

// round reads at most maxReadingsPerRound processes, one at a time, skipping
// those still held off after a failure.
func (c *Counter) round(ctx context.Context, candidates []Candidate) {
	read := 0
	for _, cand := range processes(candidates) {
		if read >= maxReadingsPerRound || ctx.Err() != nil {
			return
		}
		if c.heldOff(cand.Target) {
			continue
		}
		c.visit(ctx, cand)
		read++
	}
}

// heldOff reports whether a target is still resting after a failed reading.
func (c *Counter) heldOff(t Target) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.held[t]
	return ok && c.now().Before(until)
}

// hold rests a target, or clears its rest when the reading worked.
func (c *Counter) hold(t Target, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d == 0 {
		delete(c.held, t)
		return
	}
	c.held[t] = c.now().Add(d)
}

// processes reduces candidates to one per process worth asking. A container
// binding several ports is one process with one goroutine count, so asking each
// port would sample the same number three times and say so three times.
//
// The lowest port wins, and sorting makes the choice stable: the same port each
// round keeps a repeated failure a fact about one endpoint.
func processes(candidates []Candidate) []Candidate {
	type key struct{ namespace, kind, name, container, digest string }
	lowest := map[key]Candidate{}
	for _, c := range candidates {
		k := key{c.Namespace, c.WorkloadKind, c.WorkloadName, c.Container, c.ImageDigest}
		if held, ok := lowest[k]; ok && held.Port <= c.Port {
			continue
		}
		lowest[k] = c
	}
	out := make([]Candidate, 0, len(lowest))
	for _, c := range lowest {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Namespace != b.Namespace:
			return a.Namespace < b.Namespace
		case a.WorkloadName != b.WorkloadName:
			return a.WorkloadName < b.WorkloadName
		case a.Container != b.Container:
			return a.Container < b.Container
		default:
			return a.Port < b.Port
		}
	})
	return out
}

// visit reads one process's count. Every exit is counted, because a workload
// with no series and no reason is the silence ADR 0054 exists to end.
func (c *Counter) visit(ctx context.Context, cand Candidate) {
	addr, pod, ok := c.replica(cand)
	if !ok {
		c.count(func(cv *CountCoverage) { cv.Unreachable++ })
		return // no addressable replica right now; not an answer about the target
	}
	observedAt := c.now()
	goroutines, err := c.read(ctx, addr)
	switch {
	case errors.Is(err, errUnreadable):
		c.hold(cand.Target, heldAfterFailure)
		c.count(func(cv *CountCoverage) { cv.Unreadable++ })
		c.logger.Warn("pprof index held no goroutine count",
			"namespace", cand.Namespace, "workload", cand.WorkloadName,
			"container", cand.Container, "error", err)
		return
	case err != nil:
		c.hold(cand.Target, heldAfterFailure)
		c.count(func(cv *CountCoverage) { cv.Unreachable++ })
		c.logger.Debug("pprof index could not be read", "error", err)
		return
	}
	c.hold(cand.Target, 0)
	c.count(func(cv *CountCoverage) { cv.Sampled++ })
	c.sink(model.GoroutineCount{
		Namespace:   cand.Namespace,
		Pod:         pod,
		Container:   cand.Container,
		Workload:    model.WorkloadRef{Kind: cand.WorkloadKind, Name: cand.WorkloadName},
		ImageDigest: cand.ImageDigest,
		ObservedAt:  observedAt,
		Goroutines:  goroutines,
	})
}

func (c *Counter) count(f func(*CountCoverage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(&c.cover)
}

// errUnreadable marks an answer that arrived and was not usable, which is a
// different fact from one that never arrived.
var errUnreadable = errors.New("pprofprobe: the index page held no goroutine count")

// read fetches the index page and returns the goroutine count it carries.
func (c *Counter) read(ctx context.Context, addr string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, countTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+indexPath, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxIndexBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%w: status %d", errUnreadable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes))
	if err != nil {
		return 0, err
	}
	// The title again, on every reading rather than on the first: a number
	// scraped out of a page that is not the pprof index is a number about
	// nothing, and the prober's answer is about a build, not about what answers
	// on that address today.
	if !bytes.Contains(body, []byte(indexTitle)) {
		return 0, fmt.Errorf("%w: not the pprof index", errUnreadable)
	}
	n, ok := goroutineCount(body)
	if !ok {
		return 0, errUnreadable
	}
	return n, nil
}

// goroutineCount extracts the count from the index page's profile table.
//
// The row is `<tr><td>N</td><td><a href='...'>goroutine</a></td></tr>`, one per
// line. Matching on the rendered text rather than on the markup keeps this
// working across the attribute quoting and link shapes Go has changed before,
// and keeps it strict about the one thing that matters: the row is the profile
// named exactly `goroutine`, not the "full goroutine stack dump" link beneath
// the table and not the description list below that.
func goroutineCount(body []byte) (int64, bool) {
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		if !bytes.Contains(line, []byte(">goroutine<")) {
			continue
		}
		text := stripTags(line)
		digits := 0
		for digits < len(text) && text[digits] >= '0' && text[digits] <= '9' {
			digits++
		}
		if digits == 0 || string(text[digits:]) != "goroutine" {
			continue
		}
		n, err := parseCount(text[:digits])
		if err != nil {
			continue
		}
		return n, true
	}
	return 0, false
}

// stripTags returns the text a row renders to, with every tag removed.
func stripTags(line []byte) []byte {
	out := make([]byte, 0, len(line))
	depth := 0
	for _, b := range line {
		switch {
		case b == '<':
			depth++
		case b == '>':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			out = append(out, b)
		}
	}
	return bytes.TrimSpace(out)
}

// parseCount reads the count, refusing anything that would not fit. A page
// claiming more goroutines than an int64 holds is not the page this parses.
func parseCount(digits []byte) (int64, error) {
	var n int64
	for _, d := range digits {
		next := n*10 + int64(d-'0')
		if next < n {
			return 0, fmt.Errorf("%w: count overflows", errUnreadable)
		}
		n = next
	}
	return n, nil
}

// Snapshot returns the current coverage.
func (c *Counter) Snapshot() CountCoverage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cover
}
