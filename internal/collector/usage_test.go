package collector

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

func (r stubResolver) LookupPodByName(namespace, name string) (WorkloadRef, bool) {
	for _, entry := range r {
		if entry.namespace == namespace && entry.name == name {
			return entry.workload, true
		}
	}
	return WorkloadRef{}, false
}

var usageTestStart = time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

func testPoller(resolver PodResolver) *UsagePoller {
	return NewUsagePoller(nil, func() []string { return nil }, resolver, nil, nil, nil)
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
	var snapshots []int64
	var closedCount int
	p := NewUsagePoller(nil, func() []string { return nil }, webResolver(),
		func(sequence int64, records []*rollup.Record) {
			snapshots = append(snapshots, sequence)
			if len(records) == 0 {
				panic("snapshot callback with no records")
			}
		},
		func(records []*rollup.Record) { closedCount += len(records) },
		nil,
	)

	at := usageTestStart.Add(30 * time.Second)
	p.ingest(summaryWith(cpuMemStats(usageTestStart, at, 15e9, 64<<20)), at)

	// Window still open: snapshot only, with an increasing sequence.
	p.flush(at.Add(time.Minute))
	p.flush(at.Add(2 * time.Minute))
	if len(snapshots) != 2 || snapshots[0] != 1 || snapshots[1] != 2 {
		t.Fatalf("snapshot sequences = %v, want [1 2]", snapshots)
	}
	if closedCount != 0 {
		t.Fatalf("closed %d records while the window is open", closedCount)
	}

	// Past the window end the record closes and leaves the open set.
	p.flush(usageTestStart.Add(usageWindowLength + time.Minute))
	if closedCount != 1 {
		t.Fatalf("closed %d records, want 1", closedCount)
	}
	if len(snapshots) != 2 {
		t.Fatalf("empty open set still produced a snapshot: %v", snapshots)
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
