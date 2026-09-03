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
	"k8s.io/utils/ptr"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
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
	LookupPod(uid types.UID) (namespace string, workload model.WorkloadRef, ok bool)
	// HostNetwork reports whether the pod shares its node's network namespace.
	// The network counters mean something different when it does, and only the
	// pod spec says which (ADR 0053 §2).
	HostNetwork(uid types.UID) bool
	LookupPodByName(namespace, name string) (uid types.UID, workload model.WorkloadRef, ok bool)
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

	// requestTimeout bounds one kubelet read; a field rather than a constant
	// so a test can hang a kubelet without hanging for kubeletRequestTimeout.
	requestTimeout time.Duration

	onSnapshot func(records []*rollup.Record)
	onClosed   func(records []*rollup.Record)
	onNetwork  func(records []*rollup.NetworkRecord)
	onError    func(node string, err error)

	// The accumulator and trackers are owned by the Run goroutine.
	acc      *rollup.Accumulator
	netAcc   *rollup.NetworkAccumulator
	tracker  map[trackerKey]*counterState
	throttle map[trackerKey]*throttleState
	// netTracker is keyed by pod, not by container: one network namespace per
	// pod, so there is nothing finer to baseline (ADR 0053 §1).
	netTracker map[types.UID]*netCounterState

	// Observation state: which kubelet signals this cluster actually exposes
	// (runtime probing, ADR 0006) and how the polling itself is going. Written
	// by the poll goroutine, read by whoever ships or logs a payload, so it is
	// the one part of the poller that is synchronized.
	obsMu          sync.Mutex
	signals        map[string]bool
	pollsAttempted int64
	pollsFailed    int64
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

// netCounterState is the per-pod baseline for the network counters. Simpler
// than its per-container sibling: a pod's network namespace lives as long as the
// pod, so a container restart does not reset these — only a pod replacement
// does, and that arrives under a new UID.
type netCounterState struct {
	at       time.Time
	rxBytes  uint64
	txBytes  uint64
	rxErrors uint64
	txErrors uint64
	lastSeen time.Time
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
	onNetwork func(records []*rollup.NetworkRecord),
	onError func(node string, err error),
) *UsagePoller {
	return &UsagePoller{
		clientset:      clientset,
		nodes:          nodes,
		pods:           pods,
		requestTimeout: kubeletRequestTimeout,

		onSnapshot: onSnapshot,
		onClosed:   onClosed,
		onNetwork:  onNetwork,
		onError:    onError,
		acc:        rollup.NewAccumulator(UsageWindowLength),
		netAcc:     rollup.NewNetworkAccumulator(UsageWindowLength),
		tracker:    make(map[trackerKey]*counterState),
		throttle:   make(map[trackerKey]*throttleState),
		netTracker: make(map[types.UID]*netCounterState),
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
func (p *UsagePoller) Observation() model.Observation {
	p.obsMu.Lock()
	defer p.obsMu.Unlock()
	return model.Observation{
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

// nodeReads is what one node's two kubelet paths returned. The paths fail
// independently, so each carries its own error.
type nodeReads struct {
	node        string
	summary     *statsapi.Summary
	samples     map[cadvisorKey]*cadvisorSample
	summaryErr  error
	cadvisorErr error
}

// pollOnce reads every node and ingests the results. A failing node is
// reported and skipped: its counters are cumulative, so the next successful
// poll recovers the full interval.
func (p *UsagePoller) pollOnce(ctx context.Context, now time.Time) {
	reads := p.fetchAll(ctx, p.nodes())
	if ctx.Err() != nil {
		return // shutting down: these are cancellations, not kubelet failures
	}
	for i := range reads {
		r := &reads[i]
		p.countPoll(r.summaryErr != nil)
		if r.summaryErr != nil {
			p.reportError(r.node, r.summaryErr)
		} else {
			p.ingest(r.summary, now)
		}
		p.countPoll(r.cadvisorErr != nil)
		if r.cadvisorErr != nil {
			p.reportError(r.node, r.cadvisorErr)
		} else {
			p.ingestCadvisor(r.samples, now)
		}
	}
	p.sweep(now)
}

// fetchAll reads the nodes concurrently and returns them in the order given.
// Only the reading is concurrent: the accumulator and the counter baselines
// have a single owner and no mutex, which is the caller's goroutine (ADR 0045).
func (p *UsagePoller) fetchAll(ctx context.Context, nodes []string) []nodeReads {
	reads := make([]nodeReads, len(nodes))
	queue := make(chan int)
	var wg sync.WaitGroup
	for range min(kubeletFetchConcurrency, len(nodes)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				r := &reads[i]
				r.node = nodes[i]
				r.summary, r.summaryErr = p.fetchSummary(ctx, r.node)
				r.samples, r.cadvisorErr = p.fetchCadvisor(ctx, r.node)
			}
		}()
	}
	for i := range nodes {
		queue <- i
	}
	close(queue)
	wg.Wait()
	return reads
}

// fetchSummary reads one kubelet's /stats/summary through the API server
// proxy — one of the exactly two stats paths the agent ever requests via
// nodes/proxy (docs/security.md §4).
func (p *UsagePoller) fetchSummary(ctx context.Context, node string) (*statsapi.Summary, error) {
	ctx, cancel := context.WithTimeout(ctx, p.requestTimeout)
	defer cancel()

	stream, err := p.clientset.CoreV1().RESTClient().Get().
		Resource("nodes").Name(node).SubResource("proxy").
		Suffix("stats/summary").Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching stats summary: %w", err)
	}
	defer func() { _ = stream.Close() }()
	raw, err := readCapped(stream, maxSummaryBytes)
	if err != nil {
		return nil, fmt.Errorf("reading stats summary: %w", err)
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
		p.ingestNetwork(types.UID(pod.PodRef.UID), rollup.NetworkKey{
			Namespace:    namespace,
			WorkloadKind: workload.Kind,
			WorkloadName: workload.Name,
		}, pod, now)
	}
}

// ingestNetwork applies the delta rules of ADR 0006 to one pod's network
// counters. They are the pod's, not any container's: every container shares the
// namespace they are counted on (ADR 0053 §1).
//
// The counters are summed across the pod's interfaces. Their names are not read
// — an interface name is cluster network topology — so what says a second
// interface exists is the count.
func (p *UsagePoller) ingestNetwork(uid types.UID, key rollup.NetworkKey, pod *statsapi.PodStats, now time.Time) {
	net := pod.Network
	if net == nil {
		return
	}
	p.markSignal("network")

	var rx, tx, rxErr, txErr uint64
	interfaces := 0
	for _, iface := range interfacesOf(net) {
		if iface.RxBytes == nil && iface.TxBytes == nil {
			continue
		}
		interfaces++
		rx += ptr.Deref(iface.RxBytes, 0)
		tx += ptr.Deref(iface.TxBytes, 0)
		rxErr += ptr.Deref(iface.RxErrors, 0)
		txErr += ptr.Deref(iface.TxErrors, 0)
	}
	if interfaces == 0 {
		return
	}

	at := net.Time.Time
	st := p.netTracker[uid]
	if st == nil {
		st = &netCounterState{}
		p.netTracker[uid] = st
	}
	st.lastSeen = now

	switch {
	case st.at.IsZero():
		// First observation of this pod: nothing to subtract from, and unlike a
		// container there is no start time to attribute the counters to — the
		// summary dates the pod's stats, not its network namespace. So this one
		// establishes the baseline and emits nothing.
	case !at.After(st.at):
		return // re-served or stale snapshot: keep the baseline, emit nothing
	case rx < st.rxBytes || tx < st.txBytes || rxErr < st.rxErrors || txErr < st.txErrors:
		// A counter ran backwards: the sandbox was recreated under the same pod
		// UID, or an interface went away. Rebaseline rather than emit a
		// negative or a false spike.
	default:
		p.netAcc.Observe(key, st.at, at, rollup.NetworkDelta{
			RxBytes:    clampToInt64(rx - st.rxBytes),
			TxBytes:    clampToInt64(tx - st.txBytes),
			RxErrors:   clampToInt64(rxErr - st.rxErrors),
			TxErrors:   clampToInt64(txErr - st.txErrors),
			Interfaces: interfaces,
		}, p.pods.HostNetwork(uid))
	}
	st.at, st.rxBytes, st.txBytes, st.rxErrors, st.txErrors = at, rx, tx, rxErr, txErr
}

// interfacesOf returns the per-interface stats to sum. The kubelet reports the
// default interface inline and repeats it in Interfaces when it reports that
// list at all, so preferring the list avoids counting it twice.
func interfacesOf(net *statsapi.NetworkStats) []statsapi.InterfaceStats {
	if len(net.Interfaces) > 0 {
		return net.Interfaces
	}
	return []statsapi.InterfaceStats{net.InterfaceStats}
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
	for uid, st := range p.netTracker {
		if st.lastSeen.Before(cutoff) {
			delete(p.netTracker, uid)
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
	// Only closed windows: a network window means something complete and
	// nothing partial. Bytes accumulate, so half an hour of them read as a rate
	// over the full hour is simply wrong, and unlike CPU nothing inside the
	// agent consumes the open window (ADR 0053 §4).
	if closed := p.netAcc.CloseBefore(now); len(closed) > 0 && p.onNetwork != nil {
		p.onNetwork(closed)
	}
}

func clampToInt64(v uint64) int64 {
	const maxInt64 = uint64(1)<<63 - 1
	if v > maxInt64 {
		return int64(maxInt64) // #nosec G115 -- clamped above
	}
	return int64(v) // #nosec G115 -- clamped above
}
