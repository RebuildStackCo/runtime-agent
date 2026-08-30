// Package pprofpull fetches a CPU profile from an endpoint pprofprobe has
// confirmed, reduces it to allowed frames, and hands the bytes on (ADR 0058).
//
// It is the one thing the agent does that a customer's process can notice: the
// request starts that process's own profiler for the duration. Everything here
// is about keeping that duration short, that event rare, and that collision
// with the customer's own profiler self-correcting.
package pprofpull

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeprofile"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofprobe"
)

// profilePath is the only path this package fetches, and it is a different
// allow-list of one from the prober's for the same reason: the mux beside it
// serves the process's own argv.
const profilePath = "/debug/pprof/profile"

const (
	// captureSeconds is how long the profiled process runs its profiler. Short
	// on purpose — the sample is still statistically useful, and every second
	// is a second the customer's own profiler cannot have (ADR 0058 §2).
	captureSeconds = 10
	// requestSlack is added to the capture for connection and encoding.
	requestSlack = 20 * time.Second
	// maxProfileBytes bounds the response. A ten-second CPU profile of a busy
	// Go service is tens of kilobytes; past this the bytes are not a profile
	// this package assumes (ADR 0045's discipline).
	maxProfileBytes = 16 << 20
	// maxPullsPerCycle bounds how many workloads one round profiles, and they
	// run one at a time — so a cycle costs the cluster at most this many
	// captureSeconds, consecutively, however many endpoints exist.
	maxPullsPerCycle = 10
	// refusedFor is how long a target is left alone after the process refused
	// to start a profiler, which nearly always means its own profiler holds
	// the one slot Go allows. Long enough not to compete for it, short enough
	// that turning that profiler off is noticed within a shift (ADR 0058 §3).
	refusedFor = 6 * time.Hour
	// unreachableFor is the same for a connection that failed, which says
	// nothing about the profiler.
	unreachableFor = 30 * time.Minute
)

// Pulled is one filtered profile and the key it belongs to. The bytes are what
// survived the allow-list; the profile as fetched exists nowhere else by the
// time this is constructed.
type Pulled struct {
	Namespace    string
	WorkloadKind string
	WorkloadName string
	Container    string
	ImageDigest  string
	Start        time.Time
	End          time.Time
	Pprof        []byte
	Dropped      nodeprofile.FilterCounters
}

// Sink takes one filtered profile. Returning an error only logs: a profile that
// could not be spooled is lost, which is the loss-harmless posture every other
// payload has (ADR 0003).
type Sink func(Pulled) error

// Coverage is what the puller has done, for the collection-coverage payload.
// Counts only, and never which workload refused (ADR 0054).
type Coverage struct {
	// Shipped is filtered profiles handed to the sink.
	Shipped int `json:"shipped"`
	// Refused is attempts the process would not start a profiler for, which is
	// what a service running its own continuous profiler does to us. Cumulative
	// like everything else in that payload, and dated by its `since`.
	Refused int `json:"refused"`
	// Unreachable is targets whose connection failed, and Invalid is profiles
	// that arrived and were not shippable — unparseable, or nothing left after
	// the allow-list.
	Unreachable int `json:"unreachable"`
	Invalid     int `json:"invalid"`
}

// Puller profiles confirmed endpoints in round-robin order.
type Puller struct {
	client *http.Client
	// filter is the configured allow-list alone, used when a build states no
	// module of its own; allowed and thirdParty rebuild it per build.
	filter     *nodeprofile.SymbolFilter
	allowed    []string
	thirdParty nodeprofile.ThirdPartyPolicy
	address    pprofprobe.Address
	sink       Sink
	logger     *slog.Logger
	now        func() time.Time

	mu    sync.Mutex
	state map[pprofprobe.Target]*targetState
	cover Coverage
}

type targetState struct {
	lastPulled time.Time
	holdUntil  time.Time
}

// New builds a puller. The client is this package's own, for the reasons
// pprofprobe's is: no proxy from the environment, no redirect, no reuse.
func New(allowed []string, thirdParty nodeprofile.ThirdPartyPolicy, address pprofprobe.Address, sink Sink, logger *slog.Logger) *Puller {
	return &Puller{
		client: &http.Client{
			Timeout: captureSeconds*time.Second + requestSlack,
			Transport: &http.Transport{
				Proxy:             nil,
				DisableKeepAlives: true,
				DialContext:       (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("pprofpull: redirect refused")
			},
		},
		filter:     nodeprofile.NewSymbolFilter(allowed, thirdParty),
		allowed:    allowed,
		thirdParty: thirdParty,
		address:    address,
		sink:       sink,
		logger:     logger,
		now:        time.Now,
		state:      map[pprofprobe.Target]*targetState{},
	}
}

// Run profiles on every tick until ctx ends. confirmed supplies the candidates
// whose endpoint the prober has confirmed; the puller never decides what is
// profilable, only which of those to visit next.
func (p *Puller) Run(ctx context.Context, interval time.Duration, confirmed func() []pprofprobe.Candidate) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.cycle(ctx, confirmed())
		}
	}
}

// cycle profiles up to maxPullsPerCycle targets, least recently profiled first,
// one at a time.
//
// Round-robin rather than ranked by consumption: a ranking profiles the same
// few workloads forever and the tail is never seen, which is the failure a
// report built only from the top cannot recover from.
func (p *Puller) cycle(ctx context.Context, candidates []pprofprobe.Candidate) {
	due := p.due(candidates)
	for i, c := range due {
		if i >= maxPullsPerCycle || ctx.Err() != nil {
			return
		}
		p.visit(ctx, c)
	}
}

// due returns the candidates not currently held off, oldest first.
func (p *Puller) due(candidates []pprofprobe.Candidate) []pprofprobe.Candidate {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]pprofprobe.Candidate, 0, len(candidates))
	for _, c := range candidates {
		if st, ok := p.state[c.Target]; ok && now.Before(st.holdUntil) {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return p.lastLocked(out[i].Target).Before(p.lastLocked(out[j].Target))
	})
	return out
}

func (p *Puller) lastLocked(t pprofprobe.Target) time.Time {
	if st, ok := p.state[t]; ok {
		return st.lastPulled
	}
	return time.Time{}
}

func (p *Puller) hold(t pprofprobe.Target, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.state[t]
	if !ok {
		st = &targetState{}
		p.state[t] = st
	}
	st.lastPulled = p.now()
	st.holdUntil = p.now().Add(d)
}

func (p *Puller) count(f func(*Coverage)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(&p.cover)
}

// visit profiles one target: fetch, filter, hand on. Every exit records what
// happened, because a target with no profile and no reason is the silence
// ADR 0054 exists to end.
func (p *Puller) visit(ctx context.Context, c pprofprobe.Candidate) {
	addr, ok := p.address(c)
	if !ok {
		return // no addressable replica right now; not an answer about the target
	}
	start := p.now()
	raw, status, err := p.fetch(ctx, addr)
	end := p.now()
	switch {
	case err != nil:
		p.hold(c.Target, unreachableFor)
		p.count(func(cv *Coverage) { cv.Unreachable++ })
		p.logger.Debug("pprof pull could not complete", "error", err)
		return
	case status != http.StatusOK:
		// Nearly always 500 "cpu profiling already in use": the service runs its
		// own continuous profiler and holds the one slot Go allows. Not retried
		// soon, and never fought over (ADR 0058 §3).
		p.hold(c.Target, refusedFor)
		p.count(func(cv *Coverage) { cv.Refused++ })
		return
	}

	filtered, dropped, err := p.filterFor(c).FilterPulled(raw)
	if err == nil {
		err = nodeprofile.Validate(filtered)
	}
	if err != nil {
		p.hold(c.Target, unreachableFor)
		p.count(func(cv *Coverage) { cv.Invalid++ })
		p.logger.Warn("pulled profile refused", "error", err)
		return
	}

	p.hold(c.Target, 0)
	if err := p.sink(Pulled{
		Namespace:    c.Namespace,
		WorkloadKind: c.WorkloadKind,
		WorkloadName: c.WorkloadName,
		Container:    c.Container,
		ImageDigest:  c.ImageDigest,
		Start:        start,
		End:          end,
		Pprof:        filtered,
		Dropped:      dropped,
	}); err != nil {
		p.logger.Error("spooling pulled profile", "error", err)
		return
	}
	p.count(func(cv *Coverage) { cv.Shipped++ })
}

// filterFor builds the allow-list this build's profile is reduced against: the
// prefixes the operator configured, plus the build's own main module.
//
// The second term is what makes the default install produce a usable profile:
// a customer's own code sits under a domain-bearing module path, so an empty
// configured list would classify all of it as third-party and redact the whole
// service layer (ADR 0011 §4 named this trap). The binary states which modules
// it was built from, so nothing has to be configured (ADR 0058 §5).
func (p *Puller) filterFor(c pprofprobe.Candidate) *nodeprofile.SymbolFilter {
	if len(c.OwnModules) == 0 {
		return p.filter
	}
	allowed := make([]string, 0, len(p.allowed)+len(c.OwnModules))
	allowed = append(allowed, p.allowed...)
	allowed = append(allowed, c.OwnModules...)
	return nodeprofile.NewSymbolFilter(allowed, p.thirdParty)
}

// fetch makes the one request and returns the body, bounded.
func (p *Puller) fetch(ctx context.Context, addr string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, captureSeconds*time.Second+requestSlack)
	defer cancel()

	url := "http://" + addr + profilePath + "?seconds=" + strconv.Itoa(captureSeconds)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProfileBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProfileBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// Snapshot returns the current coverage.
func (p *Puller) Snapshot() Coverage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cover
}
