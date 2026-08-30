package collector

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/utils/ptr"

	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// stubResolver is a fixed pod attribution table; UIDs absent from it read as
// unknown-or-excluded, exactly like PodWatcher.LookupPod.
type stubResolver map[types.UID]podIndexEntry

func (r stubResolver) LookupPod(uid types.UID) (string, WorkloadRef, bool) {
	entry, ok := r[uid]
	return entry.namespace, entry.workload, ok
}

// HostNetwork answers from the same index entry the real watcher reads, so a
// test can make a pod host-networked by setting it on the entry's info.
func (r stubResolver) HostNetwork(uid types.UID) bool {
	entry, ok := r[uid]
	return ok && entry.info.Placement.HostNetwork
}

func (r stubResolver) LookupPodByName(namespace, name string) (types.UID, WorkloadRef, bool) {
	for uid, entry := range r {
		if entry.namespace == namespace && entry.name == name {
			return uid, entry.workload, true
		}
	}
	return "", WorkloadRef{}, false
}

var usageTestStart = time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

func testPoller(resolver PodResolver) *UsagePoller {
	return NewUsagePoller(nil, func() []string { return nil }, resolver, nil, nil, nil, nil)
}

// summaryWith wraps one container's stats into a node summary for the pod
// with UID "uid-1".
func summaryWith(container statsapi.ContainerStats) *statsapi.Summary {
	return &statsapi.Summary{
		Pods: []statsapi.PodStats{{
			PodRef:     statsapi.PodReference{Name: "web-abc", Namespace: "shop", UID: "uid-1"},
			Containers: []statsapi.ContainerStats{container},
		}},
	}
}

func cpuMemStats(start time.Time, at time.Time, coreNanos, workingSet uint64) statsapi.ContainerStats {
	return statsapi.ContainerStats{
		Name:      "app",
		StartTime: metav1.NewTime(start),
		CPU: &statsapi.CPUStats{
			Time:                 metav1.NewTime(at),
			UsageCoreNanoSeconds: ptr.To(coreNanos),
		},
		Memory: &statsapi.MemoryStats{
			Time:            metav1.NewTime(at),
			WorkingSetBytes: ptr.To(workingSet),
		},
	}
}

func onlyRecord(t *testing.T, p *UsagePoller) *rollup.Record {
	t.Helper()
	records := p.acc.Snapshots()
	if len(records) != 1 {
		t.Fatalf("got %d open records, want 1: %+v", len(records), records)
	}
	return records[0]
}

func webResolver() stubResolver {
	return stubResolver{
		"uid-1": {namespace: "shop", name: "web-abc", workload: WorkloadRef{Kind: "Deployment", Name: "web"}},
	}
}

// Only the agent can know that a scrape failed; from outside the cluster a
// failed scrape and a quiet container are identical. The counters must
// therefore reach the payload, not just the log.
func TestObservationCountsEveryKubeletRequestAndItsFailures(t *testing.T) {
	cases := []struct {
		name           string
		summaryStatus  int
		cadvisorStatus int
		wantFailed     int64
	}{
		{name: "node unreachable", summaryStatus: 500, cadvisorStatus: 500, wantFailed: 2},
		// The two paths fail independently: throttling can go missing while
		// CPU keeps arriving, which is why they are counted as two requests.
		{name: "cadvisor only", summaryStatus: 200, cadvisorStatus: 500, wantFailed: 1},
		{name: "both fine", summaryStatus: 200, cadvisorStatus: 200, wantFailed: 0},
	}

	for _, c := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			status, body := c.summaryStatus, `{"pods":[]}`
			if strings.HasSuffix(r.URL.Path, "metrics/cadvisor") {
				status, body = c.cadvisorStatus, ""
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		p := NewUsagePoller(clientset, func() []string { return []string{"node-1"} },
			webResolver(), nil, nil, nil, func(string, error) {})

		p.pollOnce(context.Background(), usageTestStart)
		server.Close()

		obs := p.Observation()
		// Two kubelet paths per node — /stats/summary and /metrics/cadvisor.
		if obs.PollsAttempted != 2 {
			t.Errorf("%s: polls attempted = %d, want 2", c.name, obs.PollsAttempted)
		}
		if obs.PollsFailed != c.wantFailed {
			t.Errorf("%s: polls failed = %d, want %d", c.name, obs.PollsFailed, c.wantFailed)
		}
		if obs.PollIntervalSeconds != int64(usagePollInterval/time.Second) {
			t.Errorf("%s: poll interval = %d, want %d", c.name, obs.PollIntervalSeconds,
				int64(usagePollInterval/time.Second))
		}
	}
}

func TestObservationReportsExposedSignals(t *testing.T) {
	p := testPoller(webResolver())
	at := usageTestStart.Add(30 * time.Second)
	p.ingest(summaryWith(cpuMemStats(usageTestStart, at, 15e9, 64<<20)), at)

	obs := p.Observation()
	if !slices.Equal(obs.Signals, []string{"cpu", "memory"}) {
		t.Errorf("signals = %v, want [cpu memory] — a cluster without PSI must say so", obs.Signals)
	}
	if obs.PollsAttempted != 0 {
		t.Errorf("polls attempted = %d, want 0 (ingest is not a poll)", obs.PollsAttempted)
	}
}

func TestUsageFirstObservationCountsFromContainerStart(t *testing.T) {
	p := testPoller(webResolver())

	// 15 core-seconds over the 30 s since container start: a short-lived
	// container that survived one poll is fully attributed.
	at := usageTestStart.Add(30 * time.Second)
	p.ingest(summaryWith(cpuMemStats(usageTestStart, at, 15e9, 64<<20)), at)

	r := onlyRecord(t, p)
	if r.Key != (rollup.Key{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"}) {
		t.Fatalf("record key = %+v", r.Key)
	}
	if r.CPU.CoreNanoseconds != 15e9 {
		t.Errorf("core nanos = %d, want 15e9", r.CPU.CoreNanoseconds)
	}
	if r.CPU.Samples != 1 || r.CPU.MaxMilli != 500 {
		t.Errorf("samples/max = %d/%d, want 1/500 (15 core-s over 30 s)", r.CPU.Samples, r.CPU.MaxMilli)
	}
	if r.Memory.Samples != 1 || r.Memory.MaxBytes != 64<<20 {
		t.Errorf("memory samples/max = %d/%d, want 1/%d", r.Memory.Samples, r.Memory.MaxBytes, 64<<20)
	}
}

func TestUsageDeltaBetweenPolls(t *testing.T) {
	p := testPoller(webResolver())

	first := usageTestStart.Add(30 * time.Second)
	second := first.Add(30 * time.Second)
	p.ingest(summaryWith(cpuMemStats(usageTestStart, first, 15e9, 64<<20)), first)
	p.ingest(summaryWith(cpuMemStats(usageTestStart, second, 21e9, 96<<20)), second)

	r := onlyRecord(t, p)
	if r.CPU.CoreNanoseconds != 21e9 {
		t.Errorf("core nanos = %d, want 21e9 (15e9 + 6e9 delta)", r.CPU.CoreNanoseconds)
	}
	if r.CPU.Samples != 2 {
		t.Errorf("samples = %d, want 2", r.CPU.Samples)
	}
	// Second rate: 6 core-seconds over 30 s = 200 millicores.
	if r.CPU.MinMilli != 200 || r.CPU.MaxMilli != 500 {
		t.Errorf("min/max = %d/%d, want 200/500", r.CPU.MinMilli, r.CPU.MaxMilli)
	}
	if r.Memory.Samples != 2 || r.Memory.SumBytes != 160<<20 {
		t.Errorf("memory samples/sum = %d/%d, want 2/%d", r.Memory.Samples, r.Memory.SumBytes, 160<<20)
	}
}

func TestUsageReservedSnapshotDiscarded(t *testing.T) {
	p := testPoller(webResolver())

	at := usageTestStart.Add(30 * time.Second)
	stats := cpuMemStats(usageTestStart, at, 15e9, 64<<20)
	p.ingest(summaryWith(stats), at)
	// The kubelet re-serves the identical snapshot on the next poll; the
	// unchanged timestamps mean no new information.
	p.ingest(summaryWith(stats), at.Add(30*time.Second))

	r := onlyRecord(t, p)
	if r.CPU.Samples != 1 || r.CPU.CoreNanoseconds != 15e9 {
		t.Errorf("samples/core-nanos = %d/%d, want 1/15e9 — re-served snapshot must not count",
			r.CPU.Samples, r.CPU.CoreNanoseconds)
	}
	if r.Memory.Samples != 1 {
		t.Errorf("memory samples = %d, want 1", r.Memory.Samples)
	}
}

func TestUsageCounterResetRebaselinesWithoutSample(t *testing.T) {
	p := testPoller(webResolver())

	first := usageTestStart.Add(30 * time.Second)
	restarted := first.Add(30 * time.Second)
	after := restarted.Add(30 * time.Second)
	p.ingest(summaryWith(cpuMemStats(usageTestStart, first, 15e9, 64<<20)), first)
	// The container restarted: its counter runs from zero again. The
	// observation becomes the new baseline; nothing is emitted for it.
	p.ingest(summaryWith(cpuMemStats(restarted, restarted, 2e9, 64<<20)), restarted)
	p.ingest(summaryWith(cpuMemStats(restarted, after, 8e9, 64<<20)), after)

	r := onlyRecord(t, p)
	if r.CPU.CoreNanoseconds != 15e9+6e9 {
		t.Errorf("core nanos = %d, want 21e9 (restart baseline emits nothing, next delta 6e9)",
			r.CPU.CoreNanoseconds)
	}
	if r.CPU.Samples != 2 {
		t.Errorf("samples = %d, want 2", r.CPU.Samples)
	}
}

func TestUsageUnknownPodDeferredLosslessly(t *testing.T) {
	resolver := stubResolver{}
	p := testPoller(resolver)

	// The informer has not seen the pod yet: the sample is dropped and no
	// baseline is recorded.
	first := usageTestStart.Add(30 * time.Second)
	p.ingest(summaryWith(cpuMemStats(usageTestStart, first, 15e9, 64<<20)), first)
	if got := len(p.acc.Snapshots()); got != 0 {
		t.Fatalf("unattributable sample produced %d records, want 0", got)
	}

	// Once the pod resolves, the first observation covers the full interval
	// since container start — nothing was lost to the deferral.
	resolver["uid-1"] = podIndexEntry{namespace: "shop", workload: WorkloadRef{Kind: "Deployment", Name: "web"}}
	second := first.Add(30 * time.Second)
	p.ingest(summaryWith(cpuMemStats(usageTestStart, second, 21e9, 96<<20)), second)

	r := onlyRecord(t, p)
	if r.CPU.CoreNanoseconds != 21e9 {
		t.Errorf("core nanos = %d, want the full 21e9 since container start", r.CPU.CoreNanoseconds)
	}
}

func TestUsageSweepForgetsGoneContainers(t *testing.T) {
	p := testPoller(webResolver())

	at := usageTestStart.Add(30 * time.Second)
	p.ingest(summaryWith(cpuMemStats(usageTestStart, at, 15e9, 64<<20)), at)
	if len(p.tracker) != 1 {
		t.Fatalf("tracker has %d entries, want 1", len(p.tracker))
	}

	p.sweep(at.Add(staleTrackerAfter - time.Second))
	if len(p.tracker) != 1 {
		t.Fatal("sweep dropped a recently seen container")
	}
	p.sweep(at.Add(staleTrackerAfter + time.Second))
	if len(p.tracker) != 0 {
		t.Fatal("sweep kept a container gone past the stale cutoff")
	}
}

func TestUsageFlushEmitsClosedThenSnapshots(t *testing.T) {
	var snapshots int
	var closedCount int
	p := NewUsagePoller(nil, func() []string { return nil }, webResolver(),
		func(records []*rollup.Record) {
			snapshots++
			if len(records) == 0 {
				panic("snapshot callback with no records")
			}
		},
		func(records []*rollup.Record) { closedCount += len(records) },
		nil,
		nil,
	)

	at := usageTestStart.Add(30 * time.Second)
	p.ingest(summaryWith(cpuMemStats(usageTestStart, at, 15e9, 64<<20)), at)

	// Window still open: every flush emits a snapshot, and each replaces its
	// predecessor under the window's key.
	p.flush(at.Add(time.Minute))
	p.flush(at.Add(2 * time.Minute))
	if snapshots != 2 {
		t.Fatalf("open window produced %d snapshots across two flushes, want 2", snapshots)
	}
	if closedCount != 0 {
		t.Fatalf("closed %d records while the window is open", closedCount)
	}

	// Past the window end the record closes and leaves the open set.
	p.flush(usageTestStart.Add(UsageWindowLength + time.Minute))
	if closedCount != 1 {
		t.Fatalf("closed %d records, want 1", closedCount)
	}
	if snapshots != 2 {
		t.Fatalf("empty open set still produced a snapshot: %d total", snapshots)
	}
}

// withPSI adds PSI stall counters to both resource stats, as a kubelet with
// the KubeletPSI gate on cgroup v2 exposes them.
func withPSI(stats statsapi.ContainerStats, cpuStallNanos, memStallNanos uint64) statsapi.ContainerStats {
	stats.CPU.PSI = &statsapi.PSIStats{Some: statsapi.PSIData{Total: cpuStallNanos}}
	stats.Memory.PSI = &statsapi.PSIStats{Some: statsapi.PSIData{Total: memStallNanos}}
	return stats
}

func TestUsagePSICollectedWhereExposed(t *testing.T) {
	p := testPoller(webResolver())

	first := usageTestStart.Add(30 * time.Second)
	second := first.Add(30 * time.Second)
	// First observation: stall counters count from container start.
	p.ingest(summaryWith(withPSI(cpuMemStats(usageTestStart, first, 15e9, 64<<20), 4e6, 1e6)), first)
	// Next poll: only the deltas accrue.
	p.ingest(summaryWith(withPSI(cpuMemStats(usageTestStart, second, 21e9, 64<<20), 9e6, 3e6)), second)

	r := onlyRecord(t, p)
	if r.CPU.PSIStallNanoseconds != 9e6 {
		t.Errorf("cpu psi stall = %d, want 9e6 (4e6 from start + 5e6 delta)", r.CPU.PSIStallNanoseconds)
	}
	if r.Memory.PSIStallNanoseconds != 3e6 {
		t.Errorf("memory psi stall = %d, want 3e6 (1e6 from start + 2e6 delta)", r.Memory.PSIStallNanoseconds)
	}
	got := p.Signals()
	if len(got) != 3 || got[0] != "cpu" || got[1] != "memory" || got[2] != "psi" {
		t.Errorf("signals = %v, want [cpu memory psi]", got)
	}
}

func TestUsagePSIAbsentMeansNoSignalAndNoStall(t *testing.T) {
	p := testPoller(webResolver())
	first := usageTestStart.Add(30 * time.Second)
	p.ingest(summaryWith(cpuMemStats(usageTestStart, first, 15e9, 64<<20)), first)

	r := onlyRecord(t, p)
	if r.CPU.PSIStallNanoseconds != 0 || r.Memory.PSIStallNanoseconds != 0 {
		t.Errorf("psi stall = %d/%d without PSI stats, want 0/0",
			r.CPU.PSIStallNanoseconds, r.Memory.PSIStallNanoseconds)
	}
	for _, s := range p.Signals() {
		if s == "psi" {
			t.Error("psi signal reported by a cluster that does not expose it")
		}
	}
}

func TestUsageSignalsReportWhatWasSeen(t *testing.T) {
	p := testPoller(webResolver())
	if got := p.Signals(); len(got) != 0 {
		t.Fatalf("signals before any poll = %v, want none", got)
	}

	at := usageTestStart.Add(30 * time.Second)
	stats := statsapi.ContainerStats{
		Name:      "app",
		StartTime: metav1.NewTime(usageTestStart),
		CPU: &statsapi.CPUStats{
			Time:                 metav1.NewTime(at),
			UsageCoreNanoSeconds: ptr.To(uint64(15e9)),
		},
		// No memory stats: the signal set must reflect that.
	}
	p.ingest(summaryWith(stats), at)

	got := p.Signals()
	if len(got) != 1 || got[0] != "cpu" {
		t.Fatalf("signals = %v, want [cpu]", got)
	}
}

// summaryWithNetwork wraps one pod's network stats into a node summary for the
// pod with UID "uid-1". No containers: the counters being tested belong to the
// pod, and nothing about a container is needed to reach them.
func summaryWithNetwork(at time.Time, ifaces ...statsapi.InterfaceStats) *statsapi.Summary {
	net := &statsapi.NetworkStats{Time: metav1.NewTime(at)}
	if len(ifaces) == 1 {
		net.InterfaceStats = ifaces[0]
	} else {
		net.Interfaces = ifaces
	}
	return &statsapi.Summary{
		Pods: []statsapi.PodStats{{
			PodRef:  statsapi.PodReference{Name: "web-abc", Namespace: "shop", UID: "uid-1"},
			Network: net,
		}},
	}
}

func iface(name string, rx, tx, rxErr, txErr uint64) statsapi.InterfaceStats {
	return statsapi.InterfaceStats{
		Name:     name,
		RxBytes:  ptr.To(rx),
		TxBytes:  ptr.To(tx),
		RxErrors: ptr.To(rxErr),
		TxErrors: ptr.To(txErr),
	}
}

func onlyNetworkRecord(t *testing.T, p *UsagePoller) *rollup.NetworkRecord {
	t.Helper()
	records := p.netAcc.CloseBefore(usageTestStart.Add(3 * time.Hour))
	if len(records) != 1 {
		t.Fatalf("network records = %d, want 1", len(records))
	}
	return records[0]
}

// The first observation of a pod establishes a baseline and emits nothing.
// Unlike a container, a pod's network stats carry no start time, so there is no
// interval to attribute the counters to (ADR 0053 §1).
func TestFirstNetworkObservationOnlyBaselines(t *testing.T) {
	p := testPoller(webResolver())
	at := usageTestStart.Add(time.Minute)
	p.ingest(summaryWithNetwork(at, iface("eth0", 1000, 2000, 0, 0)), at)

	if records := p.netAcc.CloseBefore(usageTestStart.Add(3 * time.Hour)); len(records) != 0 {
		t.Errorf("records = %+v after one observation, want none", records)
	}
}

func TestNetworkDeltasAreCountedFromTheResponseTimestamps(t *testing.T) {
	p := testPoller(webResolver())
	first := usageTestStart.Add(time.Minute)
	second := first.Add(30 * time.Second)
	p.ingest(summaryWithNetwork(first, iface("eth0", 1000, 2000, 1, 2)), first)
	p.ingest(summaryWithNetwork(second, iface("eth0", 5000, 9000, 3, 2)), second)

	r := onlyNetworkRecord(t, p)
	if r.RxBytes != 4000 || r.TxBytes != 7000 {
		t.Errorf("rx/tx = %d/%d, want 4000/7000", r.RxBytes, r.TxBytes)
	}
	if r.RxErrors != 2 || r.TxErrors != 0 {
		t.Errorf("rx/tx errors = %d/%d, want 2/0", r.RxErrors, r.TxErrors)
	}
	if r.WorkloadName != "web" || r.WorkloadKind != "Deployment" {
		t.Errorf("record keyed on %s/%s, want the workload", r.WorkloadKind, r.WorkloadName)
	}
	if r.CoveredNanoseconds != int64(30*time.Second) {
		t.Errorf("covered = %d, want the 30s between the two response timestamps", r.CoveredNanoseconds)
	}
}

// A re-served snapshot carries the same timestamp. Counting it again would
// invent traffic that never happened.
func TestARepeatedNetworkSnapshotIsDiscarded(t *testing.T) {
	p := testPoller(webResolver())
	first := usageTestStart.Add(time.Minute)
	p.ingest(summaryWithNetwork(first, iface("eth0", 1000, 2000, 0, 0)), first)
	p.ingest(summaryWithNetwork(first.Add(30*time.Second), iface("eth0", 5000, 9000, 0, 0)), first.Add(30*time.Second))
	p.ingest(summaryWithNetwork(first.Add(30*time.Second), iface("eth0", 5000, 9000, 0, 0)), first.Add(time.Minute))

	if r := onlyNetworkRecord(t, p); r.RxBytes != 4000 || r.Samples != 1 {
		t.Errorf("rx/samples = %d/%d, want 4000/1 — the re-served snapshot was counted", r.RxBytes, r.Samples)
	}
}

// A counter running backwards means the sandbox was recreated. Emitting the
// difference would be a negative; emitting the new value would be a spike of
// the pod's whole lifetime attributed to one window.
func TestABackwardsNetworkCounterRebaselines(t *testing.T) {
	p := testPoller(webResolver())
	first := usageTestStart.Add(time.Minute)
	p.ingest(summaryWithNetwork(first, iface("eth0", 100000, 200000, 0, 0)), first)
	p.ingest(summaryWithNetwork(first.Add(30*time.Second), iface("eth0", 50, 60, 0, 0)), first.Add(30*time.Second))
	p.ingest(summaryWithNetwork(first.Add(time.Minute), iface("eth0", 90, 100, 0, 0)), first.Add(time.Minute))

	r := onlyNetworkRecord(t, p)
	if r.RxBytes != 40 || r.TxBytes != 40 {
		t.Errorf("rx/tx = %d/%d, want 40/40 measured from the new baseline", r.RxBytes, r.TxBytes)
	}
}

// Interfaces are summed and counted; their names are never read.
func TestEveryInterfaceIsSummedAndOnlyCounted(t *testing.T) {
	p := testPoller(webResolver())
	first := usageTestStart.Add(time.Minute)
	second := first.Add(30 * time.Second)
	p.ingest(summaryWithNetwork(first, iface("eth0", 100, 200, 0, 0), iface("net1", 10, 20, 0, 0)), first)
	p.ingest(summaryWithNetwork(second, iface("eth0", 300, 500, 0, 0), iface("net1", 40, 60, 0, 0)), second)

	r := onlyNetworkRecord(t, p)
	if r.RxBytes != 230 || r.TxBytes != 340 {
		t.Errorf("rx/tx = %d/%d, want the sum over both interfaces (230/340)", r.RxBytes, r.TxBytes)
	}
	if r.Interfaces != 2 {
		t.Errorf("interfaces = %d, want 2", r.Interfaces)
	}
}

// A host-networked pod reports the node's interfaces. The flag is what stops a
// DaemonSet's records being summed with everything else into the whole
// cluster's traffic (ADR 0053 §2).
func TestHostNetworkIsFlagged(t *testing.T) {
	resolver := stubResolver{
		"uid-1": {
			namespace: "shop", name: "web-abc",
			workload: WorkloadRef{Kind: "DaemonSet", Name: "web"},
			info:     PodInfo{Placement: Placement{HostNetwork: true}},
		},
	}
	p := testPoller(resolver)
	first := usageTestStart.Add(time.Minute)
	p.ingest(summaryWithNetwork(first, iface("eth0", 100, 200, 0, 0)), first)
	p.ingest(summaryWithNetwork(first.Add(30*time.Second), iface("eth0", 300, 500, 0, 0)), first.Add(30*time.Second))

	if r := onlyNetworkRecord(t, p); !r.HostNetwork {
		t.Error("host_network is false for a pod on the node's network namespace")
	}
}

// A pod the filter never admitted has no record, and its counters are not read:
// the lookup gates the whole pod, network included (invariant 6).
func TestAnUnknownPodContributesNoNetworkRecord(t *testing.T) {
	p := testPoller(stubResolver{})
	at := usageTestStart.Add(time.Minute)
	p.ingest(summaryWithNetwork(at, iface("eth0", 1000, 2000, 0, 0)), at)
	p.ingest(summaryWithNetwork(at.Add(30*time.Second), iface("eth0", 5000, 9000, 0, 0)), at.Add(30*time.Second))

	if records := p.netAcc.CloseBefore(usageTestStart.Add(3 * time.Hour)); len(records) != 0 {
		t.Errorf("records = %+v for an unadmitted pod, want none", records)
	}
}

// A cluster whose runtime reports no network block at all must not produce a
// "network" signal: the payload would then claim the counters were observed.
func TestNoNetworkBlockRaisesNoSignal(t *testing.T) {
	p := testPoller(webResolver())
	at := usageTestStart.Add(time.Minute)
	p.ingest(summaryWith(cpuMemStats(usageTestStart, at, 15e9, 64<<20)), at)

	for _, s := range p.Observation().Signals {
		if s == "network" {
			t.Error("the network signal is set on a cluster that reported no network stats")
		}
	}
}
