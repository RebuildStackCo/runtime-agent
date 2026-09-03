package collector

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/client-go/tools/cache"
)

// The watch-health machinery answers what `HasSynced` cannot: is this cache
// still being fed?
//
// `HasSynced` is a one-way latch, so a cache whose watch is refused after the
// first LIST keeps reporting availability while serving what it last held — the
// agent goes blind and every payload still claims to be current (ADR 0035).
// client-go offers only the failure direction (SetWatchErrorHandler), so
// recovery is inferred from silence, with the reflector's own number.

const (
	// watchStreakGap is how long without a failure means the failures stopped.
	//
	// It is the reflector's own constant, for the reflector's own reason: while
	// a watch is failing it retries at most every [30,60) seconds, so a longer
	// quiet stretch cannot be part of a live failure. client-go states the same
	// judgement as `defaultBackoffReset` — "if we don't backoff for 2min, assume
	// API server is healthy" — and there is no case for a second opinion here.
	watchStreakGap = 2 * time.Minute
	// watchFatalStreak is how long a gating cache must fail continuously before
	// the agent stops.
	//
	// Not the first error: a restart costs the in-memory windows, the restart
	// baselines and the spool (emptyDir, ADR 0026), so dying on a five-second
	// API server hiccup would cost more than the blindness it prevents. Five
	// minutes is several retries, and it bounds how long the agent can be blind
	// before it says so.
	watchFatalStreak = 5 * time.Minute
	// watchUnavailableFor is how recent a failure must be for a policy source to
	// declare itself unavailable in the payload it feeds.
	//
	// Longer than the [30,60)-second retry interval, so a live refusal is
	// declared at every capture rather than at some of them; long enough that
	// the minute-by-minute flush cannot fall in a gap. A permission restored is
	// therefore still declared unavailable for a capture or two, which is the
	// direction to be wrong in.
	watchUnavailableFor = 3 * time.Minute
	// watchCheckInterval is how often the watchdog re-reads the streaks.
	watchCheckInterval = 30 * time.Second
)

// watchLimits are the durations above, held in a field so tests can compress
// them. Production never builds one by hand.
type watchLimits struct {
	streakGap      time.Duration
	fatalStreak    time.Duration
	unavailableFor time.Duration
	checkInterval  time.Duration
}

func defaultWatchLimits() watchLimits {
	return watchLimits{
		streakGap:      watchStreakGap,
		fatalStreak:    watchFatalStreak,
		unavailableFor: watchUnavailableFor,
		checkInterval:  watchCheckInterval,
	}
}

// watchHealth is one cache's record of failing: when the current run of
// failures began, when it was last seen failing, and what it last said.
//
// The error text is kept for the log and goes nowhere near a payload. A refusal
// names the agent's own ServiceAccount and the resource class it was denied,
// never a customer object — but the rule that identities of filtered-out
// objects never leave the cluster (CLAUDE.md invariant 6) is not one to defend
// case by case, so the payload carries the source name and nothing else.
type watchHealth struct {
	mu      sync.Mutex
	gap     time.Duration
	first   time.Time
	last    time.Time
	lastErr string
}

func newWatchHealth(gap time.Duration) *watchHealth {
	return &watchHealth{gap: gap}
}

// setGap re-times an already-registered record. Only tests call it, to compress
// two minutes into milliseconds without rebuilding the informers.
func (h *watchHealth) setGap(gap time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gap = gap
}

// record notes one failed ListAndWatch. A failure further than gap from the
// previous one starts a new run rather than extending the old one.
func (h *watchHealth) record(now time.Time, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last.IsZero() || now.Sub(h.last) > h.gap {
		h.first = now
	}
	h.last = now
	if err != nil {
		h.lastErr = err.Error()
	}
}

// failingFor is how long this cache has been observed failing without pause,
// and zero when it is not currently failing.
//
// The span measured is between the first and last failure, not up to now: it
// counts only time the agent watched the cache fail, never the interval since.
// One failure is therefore never a streak, which is the intent — a single
// dropped connection is not an outage.
func (h *watchHealth) failingFor(now time.Time) (time.Duration, string) {
	if h == nil {
		return 0, ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last.IsZero() || now.Sub(h.last) > h.gap {
		return 0, ""
	}
	return h.last.Sub(h.first), h.lastErr
}

// failedWithin reports whether this cache failed at any point in the last d.
func (h *watchHealth) failedWithin(now time.Time, d time.Duration) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.last.IsZero() && now.Sub(h.last) <= d
}

// watchdog blocks until ctx is canceled, or until one of the gating caches has
// been failing for longer than limits.fatalStreak — in which case it returns
// the error that must stop the agent.
//
// Stopping is the honest response for a cache that gates a signal: the pod
// index, owner chain and node list are what every other payload is assembled
// from, so a frozen store produces ordinary-looking payloads describing a
// cluster as it was. A visibly crashing pod is better for the data.
func watchdog(ctx context.Context, now func() time.Time, limits watchLimits, gating map[string]*watchHealth) error {
	if len(gating) == 0 {
		<-ctx.Done()
		return nil
	}
	names := make([]string, 0, len(gating))
	for name := range gating {
		names = append(names, name)
	}
	sort.Strings(names)

	ticker := time.NewTicker(limits.checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			at := now()
			for _, name := range names {
				failing, lastErr := gating[name].failingFor(at)
				if failing >= limits.fatalStreak {
					return fmt.Errorf(
						"the %s watch has been failing continuously for %s and its cache can no longer be called current: %s",
						name, failing.Round(time.Second), lastErr)
				}
			}
		}
	}
}

// afterSync runs between the cache-sync wait and handler registration when set,
// receiving the informer the registration is about to be made on.
//
// Only tests set it, and only to stop that informer at that instant: the window
// where a registration is refused is otherwise reached by a race, and a race is
// not something a regression test can stand on.
type afterSync func(cache.SharedIndexInformer)

// useWatchLimits re-times an assembled watcher, for tests that cannot wait five
// minutes to watch a five-minute rule fire. It must be called before Run: the
// health records are already registered with their informers, so only their
// timings are replaced, never the wiring under test.
func (w *PodWatcher) useWatchLimits(limits watchLimits, now func() time.Time) {
	w.limits = limits
	w.now = now
	for _, h := range w.gating {
		h.setGap(limits.streakGap)
	}
	for _, source := range w.policySources {
		source.health.setGap(limits.streakGap)
	}
}

// takeWatchFailure reads the watchdog's verdict without blocking. A canceled
// run has either an error waiting or nothing to say.
func takeWatchFailure(fatal <-chan error) error {
	select {
	case err := <-fatal:
		return err
	default:
		return nil
	}
}

// registrationFailure classifies an AddEventHandler error, which an informer
// returns once it has stopped.
//
// Informers stop when the run context is canceled, so a shutdown landing
// between the sync wait and registration has its handler refused: the agent was
// asked to stop, not prevented from watching, and the wait above already draws
// that distinction. The watchdog's verdict is read first — it is the other
// reason the context is canceled, and the one the caller must return.
func registrationFailure(ctx context.Context, fatal <-chan error, resource string, err error) error {
	if verdict := takeWatchFailure(fatal); verdict != nil {
		return verdict
	}
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("register %s handler: %w", resource, err)
}

// recordWatchFailures returns the handler an informer calls when ListAndWatch
// drops, bound to one cache's health record.
//
// The cause is deliberately not examined: ADR 0033 §2 decided the agent does not
// classify why a source is unreadable. Persistence does the work classification
// would — a transient error happens once, a refusal repeats until it is fixed.
func recordWatchFailures(h *watchHealth, now func() time.Time) cache.WatchErrorHandler {
	return func(_ *cache.Reflector, err error) {
		h.record(now(), err)
	}
}

// watchInformer is the part of cache.SharedIndexInformer this file needs.
type watchInformer interface {
	SetWatchErrorHandler(cache.WatchErrorHandler) error
}

// trackWatch registers the failure handler and returns the health record.
//
// SetWatchErrorHandler refuses only on an informer that has already started,
// and every call site here runs before the factory does, so the error carries
// no information a caller could act on.
func trackWatch(informer watchInformer, gap time.Duration, now func() time.Time) *watchHealth {
	h := newWatchHealth(gap)
	_ = informer.SetWatchErrorHandler(recordWatchFailures(h, now))
	return h
}
