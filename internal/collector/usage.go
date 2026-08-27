package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// Usage collection cadences (ADR 0006). The kubelet refreshes container
// stats on a ~10 s internal cadence, so polling faster than 30 s gains
// nothing; snapshots of the open window ship once a minute so the backend is
// never more than ~1–2 minutes behind the cluster.
const (
	usagePollInterval     = 30 * time.Second
	usageSnapshotInterval = time.Minute

	// UsageWindowLength is exported because it is not only the usage cadence:
	// the journal aligns its own windows to it so a restart count is read next
	// to the CPU and memory of the same hour (ADR 0020). Two constants that
	// merely happened to match would drift.
	UsageWindowLength = time.Hour

	// staleTrackerAfter is when per-container counter state for containers
	// no longer reported by any kubelet is forgotten. Long enough to ride
	// out transient node poll failures — a kept baseline just widens the
	// next delta, losing nothing.
	staleTrackerAfter = 5 * time.Minute
)

// PodResolver attributes pods to their namespace and workload, reporting
// false for pods that are unknown or excluded — exactly PodWatcher's lookup
// methods. Name-based lookup exists for the cAdvisor exposition, which
// labels containers by pod name, not UID; it returns the UID too, so both
// kubelet sources key their counter state on the same pod identity.
type PodResolver interface {
	LookupPod(uid types.UID) (namespace string, workload WorkloadRef, ok bool)
	LookupPodByName(namespace, name string) (uid types.UID, workload WorkloadRef, ok bool)
}

// UsagePoller polls every node's kubelet through the API server proxy
// (`get nodes/proxy`) for usage counters and reduces them into mergeable
// rollup records. It calls exactly one kubelet path in this slice:
// /stats/summary. CPU is collected as cumulative counter deltas — exact
// regardless of poll cadence; memory working set is a sampled gauge.
type UsagePoller struct {
	clientset kubernetes.Interface
	nodes     func() []string
	pods      PodResolver

	onSnapshot func(records []*rollup.Record)
	onClosed   func(records []*rollup.Record)
	onError    func(node string, err error)

	// The accumulator and trackers are owned by the Run goroutine.
	acc      *rollup.Accumulator
	tracker  map[trackerKey]*counterState
	throttle map[trackerKey]*throttleState

	// Observation state: which kubelet signals this cluster actually exposes
	// (runtime probing, ADR 0006) and how the polling itself is going. Written
	// by the poll goroutine, read by whoever ships or logs a payload, so it is
	// the one part of the poller that is synchronized.
	obsMu          sync.Mutex
	signals        map[string]bool
	pollsAttempted int64
	pollsFailed    int64
}

// Observation is what the agent knows about its own collection: the polling
// cadence, how many kubelet requests it made and how many failed, and which
// signals this cluster exposes. It ships with the usage payloads (ADR 0012)
// because only the agent can know a scrape failed — from outside, a failed
// scrape and a quiet container are indistinguishable.
//
// Counters are cumulative since agent start and cluster-wide; per-record
// coverage lives in the record's own sample counts and CoveredNanoseconds.
type Observation struct {
	PollIntervalSeconds int64    `json:"poll_interval_seconds"`
	PollsAttempted      int64    `json:"polls_attempted"`
	PollsFailed         int64    `json:"polls_failed"`
	Signals             []string `json:"signals"`
}

type trackerKey struct {
	pod       types.UID
	container string
}

// counterState is the per-container baseline for cumulative counters. It
// lives only in memory: after an agent restart the first observation
// rebaselines from the container's start, so no persistent state is needed
// (loss-harmless by construction). The PSI stall counters share their
// parent stats' timestamps.
type counterState struct {
	cpuTime    time.Time
	cpuCounter uint64
	cpuPSI     uint64
	memTime    time.Time
	memPSI     uint64
	lastSeen   time.Time
}

// NewUsagePoller wires a poller to its node list, pod attribution, and
// output callbacks. Any callback may be nil. onSnapshot receives deep
// copies of the open-window records; onClosed receives final records of ended
// windows; onError reports per-node poll failures, which are routine during
// node lifecycle events.
func NewUsagePoller(
	clientset kubernetes.Interface,
	nodes func() []string,
	pods PodResolver,
	onSnapshot func(records []*rollup.Record),
	onClosed func(records []*rollup.Record),
	onError func(node string, err error),
) *UsagePoller {
	return &UsagePoller{
		clientset:  clientset,
		nodes:      nodes,
		pods:       pods,
		onSnapshot: onSnapshot,
		onClosed:   onClosed,
		onError:    onError,
		acc:        rollup.NewAccumulator(UsageWindowLength),
		tracker:    make(map[trackerKey]*counterState),
		throttle:   make(map[trackerKey]*throttleState),
		signals:    make(map[string]bool),
	}
}

// Run polls until ctx is canceled. Closed windows and open-window snapshots
// are emitted on the snapshot cadence, from the same goroutine that
// accumulates — the accumulator is never shared.
func (p *UsagePoller) Run(ctx context.Context) error {
	poll := time.NewTicker(usagePollInterval)
	defer poll.Stop()
	snapshot := time.NewTicker(usageSnapshotInterval)
	defer snapshot.Stop()

	p.pollOnce(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
			p.pollOnce(ctx, time.Now())
		case <-snapshot.C:
			p.flush(time.Now())
		}
	}
}

// Signals returns the sorted names of the kubelet signals observed on this
// cluster so far (e.g. "cpu", "memory").
func (p *UsagePoller) Signals() []string {
	p.obsMu.Lock()
	defer p.obsMu.Unlock()
	return p.signalsLocked()
}

func (p *UsagePoller) signalsLocked() []string {
	out := make([]string, 0, len(p.signals))
	for name, active := range p.signals {
		if active {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Observation returns the current collection state to ship alongside a usage
// payload.
func (p *UsagePoller) Observation() Observation {
	p.obsMu.Lock()
	defer p.obsMu.Unlock()
	return Observation{
		PollIntervalSeconds: int64(usagePollInterval / time.Second),
		PollsAttempted:      p.pollsAttempted,
		PollsFailed:         p.pollsFailed,
		Signals:             p.signalsLocked(),
	}
}

func (p *UsagePoller) markSignal(name string) {
	p.obsMu.Lock()
	p.signals[name] = true
	p.obsMu.Unlock()
}

// countPoll records one kubelet request and whether it failed. Both kubelet
// paths (/stats/summary and /metrics/cadvisor) count as requests of their own:
// they fail independently, and which one failed is resolved per record by the
// per-signal sample counts, not here.
func (p *UsagePoller) countPoll(failed bool) {
	p.obsMu.Lock()
	p.pollsAttempted++
	if failed {
		p.pollsFailed++
	}
	p.obsMu.Unlock()
}

// pollOnce fetches and ingests every node's summary. A failing node is
// reported and skipped: its counters are cumulative, so the next successful
// poll recovers the full interval.
func (p *UsagePoller) pollOnce(ctx context.Context, now time.Time) {
	for _, node := range p.nodes() {
		if ctx.Err() != nil {
			return
		}
		summary, err := p.fetchSummary(ctx, node)
		p.countPoll(err != nil)
		if err != nil {
			p.reportError(node, err)
		} else {
			p.ingest(summary, now)
		}
		samples, err := p.fetchCadvisor(ctx, node)
		p.countPoll(err != nil)
		if err != nil {
			p.reportError(node, err)
		} else {
			p.ingestCadvisor(samples, now)
		}
	}
	p.sweep(now)
}

// fetchSummary reads one kubelet's /stats/summary through the API server
// proxy — one of the exactly two stats paths the agent ever requests via
// nodes/proxy (docs/security.md §4).
func (p *UsagePoller) fetchSummary(ctx context.Context, node string) (*statsapi.Summary, error) {
	raw, err := p.clientset.CoreV1().RESTClient().Get().
		Resource("nodes").Name(node).SubResource("proxy").
		Suffix("stats/summary").Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("fetching stats summary: %w", err)
	}
	var summary statsapi.Summary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, fmt.Errorf("decoding stats summary: %w", err)
	}
	return &summary, nil
}

// ingest applies one node summary to the accumulator, container by
// container. Samples of pods that are unknown (informer lag) or excluded by
// the filter are dropped before accumulation; for unknown pods the drop is
// lossless — the counter baseline is not advanced, so the next attributed
// observation covers the full interval since container start.
func (p *UsagePoller) ingest(summary *statsapi.Summary, now time.Time) {
	for i := range summary.Pods {
		pod := &summary.Pods[i]
		namespace, workload, ok := p.pods.LookupPod(types.UID(pod.PodRef.UID))
		if !ok {
			continue
		}
		for j := range pod.Containers {
			c := &pod.Containers[j]
			key := rollup.Key{
				Namespace:    namespace,
				WorkloadKind: workload.Kind,
				WorkloadName: workload.Name,
				Container:    c.Name,
			}
			p.ingestContainer(trackerKey{pod: types.UID(pod.PodRef.UID), container: c.Name}, key, c, now)
		}
	}
}

// ingestContainer applies the delta rules of ADR 0006 to one container's
// stats: deltas are computed from the timestamps inside the kubelet
// response, never from scrape time; a re-served snapshot with an unchanged
// timestamp is discarded; a counter running backwards means container
// restart — the new value becomes the baseline and no sample is emitted;
// the first observation counts as a delta from container start, so anything
// that survives to one poll is attributed.
func (p *UsagePoller) ingestContainer(tk trackerKey, key rollup.Key, c *statsapi.ContainerStats, now time.Time) {
	st := p.tracker[tk]
	if st == nil {
		st = &counterState{}
		p.tracker[tk] = st
	}
	st.lastSeen = now

	if c.CPU != nil && c.CPU.UsageCoreNanoSeconds != nil {
		p.markSignal("cpu")
		counter, at := *c.CPU.UsageCoreNanoSeconds, c.CPU.Time.Time
		var psi uint64
		if c.CPU.PSI != nil {
			p.markSignal("psi")
			psi = c.CPU.PSI.Some.Total
		}
		advance := true
		switch {
		case st.cpuTime.IsZero():
			// First observation: the counters themselves are the deltas
			// since container start.
			p.observeCPU(key, c.StartTime.Time, at, counter)
			p.observeCPUPSI(c.CPU.PSI, key, c.StartTime.Time, at, psi)
		case !at.After(st.cpuTime):
			advance = false // re-served or stale snapshot — discard, keep baselines
		case counter < st.cpuCounter || psi < st.cpuPSI:
			// Container restarted: rebaseline, emit nothing.
		default:
			p.observeCPU(key, st.cpuTime, at, counter-st.cpuCounter)
			p.observeCPUPSI(c.CPU.PSI, key, st.cpuTime, at, psi-st.cpuPSI)
		}
		if advance {
			st.cpuTime, st.cpuCounter, st.cpuPSI = at, counter, psi
		}
	}

	if c.Memory != nil && c.Memory.WorkingSetBytes != nil {
		p.markSignal("memory")
		var psi uint64
		if c.Memory.PSI != nil {
			p.markSignal("psi")
			psi = c.Memory.PSI.Some.Total
		}
		if at := c.Memory.Time.Time; at.After(st.memTime) {
			p.acc.ObserveMemory(key, at, clampToInt64(*c.Memory.WorkingSetBytes))
			if c.Memory.PSI != nil && psi >= st.memPSI {
				from := st.memTime
				if from.IsZero() {
					from = c.StartTime.Time
				}
				delta := psi
				if !st.memTime.IsZero() {
					delta = psi - st.memPSI
				}
				if to := at; to.After(from) {
					p.acc.ObserveMemoryPSI(key, from, to, clampToInt64(delta))
				}
			}
			st.memTime, st.memPSI = at, psi
		}
	}
}

func (p *UsagePoller) observeCPU(key rollup.Key, from, to time.Time, coreNanos uint64) {
	if !to.After(from) {
		return // clock skew between kubelet fields — nothing sane to record
	}
	p.acc.ObserveCPUDelta(key, from, to, clampToInt64(coreNanos))
}

func (p *UsagePoller) observeCPUPSI(psi *statsapi.PSIStats, key rollup.Key, from, to time.Time, stallNanos uint64) {
	if psi == nil || !to.After(from) {
		return
	}
	p.acc.ObserveCPUPSI(key, from, to, clampToInt64(stallNanos))
}

func (p *UsagePoller) reportError(node string, err error) {
	if p.onError != nil {
		p.onError(node, err)
	}
}

// sweep drops counter state for containers no kubelet has reported for a
// while, so churned pods do not accumulate tracker entries forever.
func (p *UsagePoller) sweep(now time.Time) {
	cutoff := now.Add(-staleTrackerAfter)
	for tk, st := range p.tracker {
		if st.lastSeen.Before(cutoff) {
			delete(p.tracker, tk)
		}
	}
	for ck, st := range p.throttle {
		if st.lastSeen.Before(cutoff) {
			delete(p.throttle, ck)
		}
	}
}

// flush emits closed windows first, then a fresh snapshot of the still-open
// ones. Each snapshot replaces its predecessor under the window's key and
// carries nothing that orders it: the spool holds one snapshot per window, so
// there are never two to order (ADR 0027).
func (p *UsagePoller) flush(now time.Time) {
	if closed := p.acc.CloseBefore(now); len(closed) > 0 && p.onClosed != nil {
		p.onClosed(closed)
	}
	if snapshots := p.acc.Snapshots(); len(snapshots) > 0 && p.onSnapshot != nil {
		p.onSnapshot(snapshots)
	}
}

func clampToInt64(v uint64) int64 {
	const maxInt64 = uint64(1)<<63 - 1
	if v > maxInt64 {
		return int64(maxInt64) // #nosec G115 -- clamped above
	}
	return int64(v) // #nosec G115 -- clamped above
}
