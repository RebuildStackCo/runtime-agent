// Package pprofprobe confirms, once per build and port, that a workload really
// serves `/debug/pprof` — the last step of a funnel whose first three stages
// cost no network at all (ADR 0057).
//
// It is where the controller first opens a connection to something other than
// the API server. What bounds that is this package: the address comes from a pod
// the collection filters admitted, the port from one the process was observed to
// bind, and the path from the constant below.
package pprofprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/inventory"
)

// indexPath is the only path this package fetches, and the allow-list is the
// point rather than the mechanism: the pprof mux also serves
// `/debug/pprof/cmdline`, which returns the process's own argv — the field
// CLAUDE.md invariant 4 drops at the source. No code here may name it.
const indexPath = "/debug/pprof/"

// indexTitle is what Go's own index page puts in its title and nothing else
// does. A 200 alone proves only that something answered on the port.
const indexTitle = "<title>/debug/pprof/</title>"

const (
	// probeTimeout bounds one request. The index page is served from memory and
	// starts no profiler, so a slow answer is a sick workload, not a busy one.
	probeTimeout = 5 * time.Second
	// maxIndexBytes bounds the response read. The page is about 2 KiB; past
	// this it is not the page this package assumes (ADR 0045's discipline).
	maxIndexBytes = 64 << 10
	// maxProbesPerTick bounds how many new targets one round may ask about, so
	// a cluster that grows by a thousand images does not answer for all of them
	// at once.
	maxProbesPerTick = 20
	// unreachableRetry is how long a connection failure is left alone. It says
	// nothing about the endpoint — a NetworkPolicy, a mesh, a restarting pod —
	// so it expires, where the two answers about the endpoint itself do not.
	unreachableRetry = 30 * time.Minute
)

// State is what is known about one build's port.
type State int

const (
	// StateUnknown is a target not yet asked about.
	StateUnknown State = iota
	// StateConfirmed is a port that served the pprof index. Terminal: the
	// binary and the port do not change under a digest.
	StateConfirmed
	// StateAbsent is a port that answered but not with pprof. Also terminal,
	// and the common case for a workload that links the package and serves it
	// on one port out of several.
	StateAbsent
	// StateUnreachable is a port that could not be connected to. Retried.
	StateUnreachable
)

// Target is one build's port: what an answer is about, and what it is cached
// under. Not the pod — every replica of a build serves the same page, so asking
// a second one learns nothing.
type Target struct {
	ImageDigest string
	Port        int
}

// Candidate is a target together with the workload to ask. The workload fields
// are how the prober finds a pod address; they are not part of the answer.
type Candidate struct {
	Target
	Namespace    string
	WorkloadKind string
	WorkloadName string
	Container    string
	// OwnModules are the module paths the build compiles from source, read from
	// the binary. The prober does not use them; they ride here because this is
	// where the digest is joined to what is known about the build (ADR 0058 §5).
	OwnModules []string
}

// Address resolves a candidate to one reachable `host:port` to ask, or reports
// that no admitted pod of that workload currently has an address.
//
// The address never leaves the caller: a pod IP is a connection parameter, not
// a fact, and it enters no record and no payload (ADR 0057 §3).
type Address func(Candidate) (string, bool)

// Prober asks each target once and remembers the answer. Safe for concurrent
// use: the run loop writes, the coverage and endpoint readers read.
type Prober struct {
	client  *http.Client
	address Address
	logger  *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	answers map[Target]answer
}

type answer struct {
	state   State
	retryAt time.Time // zero unless the state expires
}

// New builds a prober. The client is this package's own: no proxy from the
// environment, no redirect followed, no connection reused — a probe is one
// request to one address and must not become a request to another.
func New(address Address, logger *slog.Logger) *Prober {
	return &Prober{
		client: &http.Client{
			Timeout: probeTimeout,
			Transport: &http.Transport{
				Proxy:             nil,
				DisableKeepAlives: true,
				DialContext:       (&net.Dialer{Timeout: probeTimeout}).DialContext,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("pprofprobe: redirect refused")
			},
		},
		address: address,
		logger:  logger,
		now:     time.Now,
		answers: map[Target]answer{},
	}
}

// Run probes new candidates on every tick until ctx ends. Candidates are
// supplied by the caller rather than held, so the funnel upstream stays the one
// place that decides what is worth asking about.
func (p *Prober) Run(ctx context.Context, interval time.Duration, candidates func() []Candidate) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.round(ctx, candidates())
		}
	}
}

// round asks about at most maxProbesPerTick targets, one at a time. Sequential
// on purpose: the cost of being slow is a later answer, and the cost of being
// parallel is a burst of connections into the customer's workloads.
func (p *Prober) round(ctx context.Context, candidates []Candidate) {
	asked := 0
	for _, c := range candidates {
		if asked >= maxProbesPerTick {
			return
		}
		if !p.due(c.Target) {
			continue
		}
		addr, ok := p.address(c)
		if !ok {
			continue // no admitted pod with an address right now; ask again later
		}
		state := p.ask(ctx, addr)
		p.record(c.Target, state)
		asked++
		if ctx.Err() != nil {
			return
		}
	}
}

// due reports whether a target still needs an answer.
func (p *Prober) due(t Target) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.answers[t]
	if !ok {
		return true
	}
	return !a.retryAt.IsZero() && !p.now().Before(a.retryAt)
}

func (p *Prober) record(t Target, state State) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := answer{state: state}
	if state == StateUnreachable {
		a.retryAt = p.now().Add(unreachableRetry)
	}
	p.answers[t] = a
}

// ask makes the one request. Errors are states, not failures: a workload that
// cannot be reached is a fact about the cluster, and the caller has nothing to
// do with an error value it would only log.
func (p *Prober) ask(ctx context.Context, addr string) State {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	url := "http://" + addr + indexPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return StateUnreachable
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.logger.Debug("pprof probe could not connect", "error", err)
		return StateUnreachable
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxIndexBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return StateAbsent
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes))
	if err != nil {
		return StateUnreachable
	}
	// The title rather than the status: matching on 200 alone would confirm any
	// handler that answers on a path it does not know.
	if !bytes.Contains(body, []byte(indexTitle)) {
		return StateAbsent
	}
	return StateConfirmed
}

// Endpoints returns the confirmed targets, for whatever pulls from them.
func (p *Prober) Endpoints() []Target {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Target, 0, len(p.answers))
	for t, a := range p.answers {
		if a.state == StateConfirmed {
			out = append(out, t)
		}
	}
	return out
}

// Confirmed narrows candidates to those whose endpoint answered with the pprof
// index. It is the whole of what this package tells the puller: where an
// endpoint is, never whether it is worth profiling.
func (p *Prober) Confirmed(candidates []Candidate) []Candidate {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if a, ok := p.answers[c.Target]; ok && a.state == StateConfirmed {
			out = append(out, c)
		}
	}
	return out
}

// Coverage is what the prober has established, for the collection-coverage
// payload. Counts only: which workload was asked about is already in the
// inventory beside it, and which was refused is not a name this reports
// (ADR 0054).
type Coverage struct {
	// Confirmed, Absent and Unreachable are targets by their latest answer.
	Confirmed   int `json:"confirmed"`
	Absent      int `json:"absent"`
	Unreachable int `json:"unreachable"`
}

// Snapshot returns the current coverage.
func (p *Prober) Snapshot() Coverage {
	p.mu.Lock()
	defer p.mu.Unlock()
	var c Coverage
	for _, a := range p.answers {
		switch a.state {
		case StateConfirmed:
			c.Confirmed++
		case StateAbsent:
			c.Absent++
		case StateUnreachable:
			c.Unreachable++
		case StateUnknown:
		}
	}
	return c
}

// HostPort joins a host and port for Address implementations.
func HostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// String makes a state legible in a log line.
func (s State) String() string {
	switch s {
	case StateConfirmed:
		return "confirmed"
	case StateAbsent:
		return "absent"
	case StateUnreachable:
		return "unreachable"
	case StateUnknown:
		return "unknown"
	}
	return fmt.Sprintf("state(%d)", int(s))
}

// Candidates joins the two node-read facts into the set worth asking about: a
// build that links the package, on a port its processes were observed to bind.
// Three stages of the funnel have run before this one, none of them costing a
// byte of network (ADR 0057 §1).
//
// A loopback port is dropped rather than asked about: nothing outside the pod
// can open it, so a probe would prove only that the agent is not inside.
func Candidates(ports []inventory.PortRecord, builds map[string][]string) []Candidate {
	var out []Candidate
	for _, rec := range ports {
		ownModules, ok := builds[rec.ImageDigest]
		if !ok {
			continue
		}
		for _, port := range rec.Ports {
			if port.Loopback {
				continue
			}
			out = append(out, Candidate{
				Target:       Target{ImageDigest: rec.ImageDigest, Port: port.Port},
				Namespace:    rec.Namespace,
				WorkloadKind: rec.WorkloadKind,
				WorkloadName: rec.WorkloadName,
				Container:    rec.Container,
				OwnModules:   ownModules,
			})
		}
	}
	return out
}
