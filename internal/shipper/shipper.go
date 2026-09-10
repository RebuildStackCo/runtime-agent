// Package shipper delivers the payloads the spool holds to a backend, and
// deletes a file only once the backend has acknowledged it.
//
// The spool is the queue: its filenames are the natural keys and deletion is
// acknowledgment (ADR 0003), so nothing here is state a restart would need
// back. One goroutine with one request in flight is what keeps at most one
// request per natural key (ADR 0027 §2) and keeps a fleet from being the
// reason a recovering backend goes down again (ADR 0075).
package shipper

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

// MaxPayloadBytes is the largest payload this agent will send. A file past it
// is refused here, counted as `too_large`, and never offered — the same outcome
// as a 413, reached without spending the request.
//
// It is a placeholder, and the counter is how its wrongness becomes visible:
// `too_large` rising with nothing else wrong means a cluster produces payloads
// this number does not fit. The protocol's real limit is unsettled and belongs
// to the registry as a property of a kind, beside cadence (ADR 0073, ADR 0075).
const MaxPayloadBytes = 4 << 20

// DefaultInterval is how often a round runs while deliveries are getting
// through. A round with an empty spool is one directory listing.
const DefaultInterval = 30 * time.Second

const (
	// requestTimeout bounds one delivery. The same 30 seconds the node→
	// controller shippers use; nothing here is streaming.
	requestTimeout = 30 * time.Second
	// The backoff after a round that could not deliver: doubling from minBackoff
	// to maxBackoff, then jittered.
	minBackoff = 5 * time.Second
	maxBackoff = 5 * time.Minute
	// responseDrainBytes is how much of a response body is read before it is
	// discarded, so the connection can be reused.
	responseDrainBytes = 4 << 10
)

// Shipper posts the spool's payloads to one backend.
type Shipper struct {
	base   string
	dir    string
	client *http.Client
	logger *slog.Logger

	// halted latches on an identity failure and never clears: nothing retries
	// into a rejected credential.
	halted atomic.Bool

	// mu guards everything below. The round runs on one goroutine; the lock is
	// for the coverage flush and the metrics handler reading across it.
	mu sync.Mutex
	// held is the payloads this process will not offer again — a 400, a 413, or
	// a file past MaxPayloadBytes. Keyed by name and matched by file identity,
	// so a superseding writer replacing the file clears the mark and the new
	// version is tried. In memory only: a restart offers them once more, which
	// is the safe direction and self-limiting.
	held map[string]os.FileInfo
	// failures is consecutive rounds that could not deliver, which is what the
	// backoff grows on.
	failures int
	counts   model.Shipping

	// interval and jitter are fields only so a test can compress the first and
	// fix the second. Production assigns neither.
	interval time.Duration
	jitter   func(n int64) int64
}

// New returns a shipper for base, or nil when base is empty — the unconfigured
// agent, which collects and spools exactly as it always has.
func New(base, dir string, logger *slog.Logger) *Shipper {
	if base == "" {
		return nil
	}
	return &Shipper{
		base:   strings.TrimRight(base, "/"),
		dir:    dir,
		client: &http.Client{Timeout: requestTimeout},
		logger: logger,
		held:   map[string]os.FileInfo{},
		// #nosec G404 -- jitter spreads a fleet's retries over a window; it is
		// not a secret and predicting it buys nothing
		jitter:   rand.Int64N,
		interval: DefaultInterval,
	}
}

// Run ships until ctx is canceled.
//
// It returns only on cancellation, halted or not: in cmd/agent a lifecycle task
// that returns stops the whole agent, and collection does not depend on
// shipping. A backend that is unreachable, hostile or absent changes nothing
// about what this process collects or writes.
func (s *Shipper) Run(ctx context.Context) error {
	s.logger.Info("shipping enabled", "backend", s.base, "spool", s.dir)
	for {
		timer := time.NewTimer(s.nextDelay())
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		s.Round(ctx)
		if s.halted.Load() {
			<-ctx.Done()
			return nil
		}
	}
}

// Round offers every payload the spool holds, oldest first, one at a time.
//
// Oldest first is the sweep's own eviction order (ADR 0042): what is closest to
// being dropped for age is what is closest to being delivered. The round stops
// at the first payload the backend could not take, because the next one would
// meet the same backend.
func (s *Shipper) Round(ctx context.Context) {
	if s.halted.Load() {
		return // nothing retries into a rejected credential
	}
	for _, f := range s.pending() {
		if ctx.Err() != nil {
			return
		}
		if !s.deliver(ctx, f) {
			return
		}
	}
}

// Coverage is what shipping did, for the coverage payload and the metrics
// endpoint. Both read this one snapshot, so the two cannot disagree
// (ADR 0070 §2).
func (s *Shipper) Coverage() model.Shipping {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.counts
	c.Halted = s.halted.Load()
	return c
}

// spooled is one candidate file: its name and the modification time the round
// orders by.
type spooled struct {
	name    string
	modTime time.Time
}

// pending lists the payloads to offer this round and forgets the marks on files
// that are gone or have been replaced. A `.tmp` file is a write in progress —
// the publish is a rename, so every other name in the directory is a complete
// payload (ADR 0003).
func (s *Shipper) pending() []spooled {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		s.logger.Error("listing the spool failed; nothing shipped this round", "error", err)
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	still := make(map[string]os.FileInfo, len(s.held))
	var out []spooled
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue // raced with a rename or a delete; the next round sees the truth
		}
		if prev, ok := s.held[name]; ok && os.SameFile(prev, info) {
			still[name] = prev
			continue
		}
		out = append(out, spooled{name: name, modTime: info.ModTime()})
	}
	s.held = still

	sort.Slice(out, func(i, j int) bool {
		if out[i].modTime.Equal(out[j].modTime) {
			return out[i].name < out[j].name
		}
		return out[i].modTime.Before(out[j].modTime)
	})
	return out
}

// deliver offers one payload and reports whether the round continues.
func (s *Shipper) deliver(ctx context.Context, f spooled) bool {
	data, info, err := s.read(f.name)
	switch {
	case err != nil:
		// Nothing is deleted on the strength of a failed read: a payload this
		// process cannot read is still one the backend could ingest, and the
		// spool's own bounds are what remove it.
		s.logger.Error("spool payload could not be read; leaving it where it is",
			"file", f.name, "error", err)
		s.count(func(c *model.Shipping) { c.Unreadable++ })
		return true
	case info.Size() > MaxPayloadBytes || len(data) > MaxPayloadBytes:
		s.logger.Error("payload is past the agent's size ceiling and will not be offered",
			"file", f.name, "bytes", info.Size(), "ceiling", MaxPayloadBytes)
		s.hold(f.name, info, func(c *model.Shipping) { c.Rejected.TooLarge++ })
		return true
	}

	kind, err := payloadKind(data)
	if err != nil {
		s.logger.Error("spool payload names no kind this agent ships; leaving it where it is",
			"file", f.name, "error", err)
		s.count(func(c *model.Shipping) { c.Unreadable++ })
		return true
	}

	status, err := s.post(ctx, kind, data)
	if err != nil {
		if ctx.Err() != nil {
			return false // shutting down: the file is the truth, and it stays
		}
		s.logger.Warn("delivery failed; the payload stays in the spool",
			"file", f.name, "kind", kind, "error", err)
		s.deferred()
		return false
	}

	switch {
	case status >= 200 && status < 300:
		s.acknowledged(f.name, info, kind)
		return true

	// An identity problem is the one answer with nothing to retry into, and a
	// fleet retrying into it is how the thing it is trying to reach stays down.
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		s.logger.Error("the backend refused this agent's identity; shipping stops until this agent is restarted",
			"file", f.name, "kind", kind, "status", status)
		s.count(func(c *model.Shipping) { c.Rejected.Unauthorized++ })
		s.halted.Store(true)
		return false

	// Permanent for these bytes: held, never offered again, and left in the
	// spool until its age bound removes it. Holding rather than deleting is what
	// keeps the payload that proves the disagreement readable while someone
	// looks at it (ADR 0075).
	case status == http.StatusBadRequest:
		s.logger.Error("the backend will not accept this payload in this form; it will not be offered again",
			"file", f.name, "kind", kind, "status", status)
		s.hold(f.name, info, func(c *model.Shipping) { c.Rejected.Malformed++ })
		return true
	case status == http.StatusRequestEntityTooLarge:
		s.logger.Error("the backend refused this payload as too large; it will not be offered again",
			"file", f.name, "kind", kind, "status", status, "bytes", info.Size())
		s.hold(f.name, info, func(c *model.Shipping) { c.Rejected.TooLarge++ })
		return true

	// Everything else is transient, deliberately including a status this agent
	// has no rule for. A base URL with a typo answers 404 to every payload, and
	// treating an unrecognized answer as permanent would empty the spool against
	// a configuration mistake.
	default:
		s.logger.Warn("the backend did not take this payload; it stays in the spool",
			"file", f.name, "kind", kind, "status", status)
		s.deferred()
		return false
	}
}

// acknowledged removes a delivered payload — but only if the file is still the
// one that was sent.
//
// A superseding writer replaces a file by rename while a request is in flight
// (ADR 0003), so deleting by name alone would drop a newer payload that was
// never offered. The newer one goes out on the next round.
func (s *Shipper) acknowledged(name string, sent os.FileInfo, kind string) {
	s.mu.Lock()
	s.counts.Delivered++
	s.failures = 0
	s.mu.Unlock()

	path := filepath.Join(s.dir, name)
	current, err := os.Stat(path)
	if err != nil {
		return // already gone: the sweep, or a writer between the two
	}
	if !os.SameFile(sent, current) {
		s.logger.Info("a newer payload replaced the delivered one; it ships next round",
			"file", name, "kind", kind)
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.logger.Error("removing a delivered payload failed; it will be delivered again",
			"file", name, "error", err)
	}
}

// hold marks a payload as one this process will not offer again, and counts why.
func (s *Shipper) hold(name string, info os.FileInfo, count func(*model.Shipping)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[name] = info
	count(&s.counts)
}

// deferred records an attempt that left the payload where it was, and grows the
// backoff.
func (s *Shipper) deferred() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts.Deferred++
	s.failures++
}

func (s *Shipper) count(f func(*model.Shipping)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.counts)
}

// read opens one payload and returns its bytes and the identity of the file
// they came from. The stat is taken from the open descriptor, so it names the
// bytes that were read and not whatever holds the name afterwards.
//
// The read is bounded one byte past the ceiling, which is all the caller needs
// to know the payload is past it.
func (s *Shipper) read(name string) ([]byte, os.FileInfo, error) {
	f, err := os.Open(filepath.Join(s.dir, name)) // #nosec G304 -- a plain filename inside the agent's own spool
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Size() > MaxPayloadBytes {
		return nil, info, nil // past the ceiling: not read, and not offered
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxPayloadBytes+1))
	if err != nil {
		return nil, nil, err
	}
	return data, info, nil
}

// post delivers one payload and returns the status.
//
// Nothing is read out of a response but its status: the body is drained so the
// connection can be reused and discarded unparsed. ADR 0001 forbids the backend
// to send anything the agent acts on, and this is the line where that stops
// being a promise about the backend and becomes a property of the agent.
func (s *Shipper) post(ctx context.Context, kind string, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	// The address is the operator's own configuration and the path segment is a
	// registry name that payloadKind already checked, so neither half is
	// caller-influenced input.
	endpoint := s.base + "/v1/kubernetes/" + url.PathEscape(kind)
	encoded, err := encode(body)
	if err != nil {
		return 0, fmt.Errorf("encoding payload: %w", err)
	}
	s.count(func(c *model.Shipping) {
		c.PayloadBytes += uint64(len(body))
		c.TransmittedBytes += uint64(len(encoded))
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded)) // #nosec G704 -- endpoint is operator-set config plus a registry name, not tainted input
	if err != nil {
		return 0, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Unconditional, never negotiated: the agent cannot ask what the backend
	// accepts and nothing it answers may change agent behaviour (ADR 0001), so
	// the encoding is a property of the protocol version (ADR 0076).
	req.Header.Set("Content-Encoding", "gzip")

	resp, err := s.client.Do(req) // #nosec G704 -- as above
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, responseDrainBytes))
	return resp.StatusCode, nil
}

// encode is the transport encoding, and the payload is what it encodes:
// gunzip of what this returns is body, byte for byte, which is what keeps the
// local sink and the wire the same payload (ADR 0076).
//
// Level 6 measured 11.4x on a thousand-record usage snapshot for 8.7 ms.
func encode(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(body); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// nextDelay is how long before the next round: the interval while deliveries
// land, and a growing backoff while they do not.
//
// The jitter is the half that matters at fleet scale. After an outage every
// agent has a full spool and the same idea at the same moment; the backend is
// required to tolerate a catch-up burst (backend-requirements.md §4) and the
// agent is required not to make it a stampede.
func (s *Shipper) nextDelay() time.Duration {
	s.mu.Lock()
	failures := s.failures
	s.mu.Unlock()
	if failures == 0 {
		return s.interval
	}
	d := minBackoff
	for i := 1; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	// Half fixed, half spread: enough randomness to break a fleet's lockstep,
	// not so much that a recovered backend waits on an agent that drew short.
	return d/2 + time.Duration(s.jitter(int64(d/2)))
}

// payloadKind reads the kind a payload declares and refuses one the registry
// does not hold. The kind is the last path segment of the endpoint, so this is
// also what keeps that segment a name the agent published rather than whatever
// a file in the directory happens to contain (ADR 0022).
func payloadKind(data []byte) (string, error) {
	var envelope struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", fmt.Errorf("decoding the payload envelope: %w", err)
	}
	if envelope.Kind == "" {
		return "", fmt.Errorf("the payload declares no kind")
	}
	if _, ok := sink.Lookup(envelope.Kind); !ok {
		return "", fmt.Errorf("kind %q has no row in the registry", envelope.Kind)
	}
	return envelope.Kind, nil
}
