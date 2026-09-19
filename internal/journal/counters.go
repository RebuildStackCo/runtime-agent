package journal

import (
	"sort"
	"sync"
	"time"
)

// The agent's own counters, as change rather than as a total. Every number in
// `collection_coverage` is cumulative from `since`, which answers how much and
// never when: a namespace annotated at 14:00 and one annotated three weeks ago
// reach the same total. This journal is the other half, on the same terms
// ADR 0088 set for the states beside it (ADR 0089).

// CounterRecord is one counter's change within one window.
//
// Delta and ObservedNanos are both needed for the same reason the state records
// carry occupancy and observation: a window the agent was absent for half of
// has a smaller delta for reasons that are about the agent, not the cluster.
type CounterRecord struct {
	// Counter is the dotted path the same number has where it is reported
	// cumulatively — `filter.excluded_pod_annotation`, `pprof_pull.refused`.
	// The name is the join, so nothing here restates what that number means.
	Counter string `json:"counter"`

	WindowStart   time.Time `json:"window_start"`
	WindowSeconds int64     `json:"window_seconds"`

	// Delta is how much the counter rose within this window.
	Delta int64 `json:"delta"`
	// ObservedNanos is how much of the window the agent was running to count.
	ObservedNanos int64 `json:"observed_nanos"`
	// AccruedTo is the instant both numbers above are true up to, and what a
	// later process resumes from (ADR 0089 §3).
	AccruedTo time.Time `json:"accrued_to"`
}

type counterKey struct {
	counter string
	start   int64
}

// Counters accumulates the change in the agent's own counters into
// wall-clock-aligned windows, the same windows the state records use.
type Counters struct {
	mu           sync.Mutex
	windowLength time.Duration
	open         map[counterKey]*CounterRecord
	// prev is the previous pass's readings, and prevSince the base they were
	// counted from. A reading is only subtractable from one counted on the same
	// base, which is what makes a restart produce no delta rather than a wrong
	// one (ADR 0062 §1).
	prev      map[string]int64
	prevSince time.Time
	prevAt    time.Time
}

// NewCounters returns an accumulator over windows of the given length, which
// must be positive.
func NewCounters(windowLength time.Duration) *Counters {
	if windowLength <= 0 {
		panic("journal: window length must be positive")
	}
	return &Counters{
		windowLength: windowLength,
		open:         map[counterKey]*CounterRecord{},
		prev:         map[string]int64{},
	}
}

// Observe folds the change since the previous pass into the window holding at.
// since is the base the readings are counted from, and readings are the totals
// as this pass found them.
//
// A pass whose base differs from the previous one accrues nothing: the previous
// process's readings are not subtractable from this one's, and what it counted
// is already in the record on disk.
func (c *Counters) Observe(at, since time.Time, readings map[string]int64) {
	at = at.UTC()
	c.mu.Lock()
	defer c.mu.Unlock()

	sameBase := !c.prevSince.IsZero() && c.prevSince.Equal(since)
	if sameBase && at.After(c.prevAt) {
		c.accrue(at, readings)
	}
	for name := range readings {
		c.record(name, at).AccruedTo = at
	}
	c.prev, c.prevSince, c.prevAt = copyReadings(readings), since, at
}

// accrue attributes the interval and the rises across it.
//
// The interval is split at any window boundary it crosses, because it is a
// duration. The rise is not: it happened at some instant this pass cannot see,
// so it lands whole in the window the pass observed it in (ADR 0089 §4).
func (c *Counters) accrue(at time.Time, readings map[string]int64) {
	for _, s := range c.split(c.prevAt, at) {
		for name := range readings {
			c.record(name, s.start).ObservedNanos += s.end.Sub(s.start).Nanoseconds()
		}
	}
	for name, now := range readings {
		was, known := c.prev[name]
		if !known || now < was {
			continue // a counter that fell is not a negative rise, it is a new base
		}
		c.record(name, at).Delta += now - was
	}
}

// record returns the open record for one counter in the window holding at.
func (c *Counters) record(name string, at time.Time) *CounterRecord {
	start := at.UTC().Truncate(c.windowLength)
	key := counterKey{counter: name, start: start.UnixNano()}
	rec := c.open[key]
	if rec == nil {
		rec = &CounterRecord{
			Counter:       name,
			WindowStart:   start,
			WindowSeconds: int64(c.windowLength / time.Second),
		}
		c.open[key] = rec
	}
	return rec
}

func (c *Counters) split(from, to time.Time) []span {
	var out []span
	for from.Before(to) {
		end := from.Truncate(c.windowLength).Add(c.windowLength)
		if end.After(to) {
			end = to
		}
		out = append(out, span{start: from, end: end})
		from = end
	}
	return out
}

// Snapshots returns deep copies of every open record, sorted so the payload
// bytes are deterministic (the golden contract).
func (c *Counters) Snapshots() []CounterRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CounterRecord, 0, len(c.open))
	for _, rec := range c.open {
		out = append(out, *rec)
	}
	sortCounterRecords(out)
	return out
}

// Resume seeds open windows from records a previous process wrote (ADR 0077).
// A key already present is left alone.
//
// It seeds records and not readings: what the previous process last read is not
// in the payload and is not needed there, because the first pass of this one
// accrues nothing anyway.
func (c *Counters) Resume(records []CounterRecord) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var seeded int
	for _, rec := range records {
		key := counterKey{counter: rec.Counter, start: rec.WindowStart.UnixNano()}
		if _, ok := c.open[key]; ok {
			continue
		}
		resumed := rec
		c.open[key] = &resumed
		seeded++
	}
	return seeded
}

// CloseBefore removes and returns every record whose window ended at or before
// now, which is what bounds memory.
func (c *Counters) CloseBefore(now time.Time) []CounterRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []CounterRecord
	for key, rec := range c.open {
		if rec.WindowStart.Add(c.windowLength).After(now) {
			continue
		}
		out = append(out, *rec)
		delete(c.open, key)
	}
	sortCounterRecords(out)
	return out
}

func copyReadings(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func sortCounterRecords(records []CounterRecord) {
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if !a.WindowStart.Equal(b.WindowStart) {
			return a.WindowStart.Before(b.WindowStart)
		}
		return a.Counter < b.Counter
	})
}
