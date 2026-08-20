package collector

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// fixture builds a cAdvisor text exposition for one container plus the rows
// the parser must ignore: pod-level cgroups (empty container label), pause
// containers ("POD"), and unrelated metric families.
func fixture(throttled, periods uint64, at, start time.Time) string {
	ms := at.UnixMilli()
	return fmt.Sprintf(`# HELP container_cpu_cfs_periods_total Number of elapsed enforcement period intervals.
# TYPE container_cpu_cfs_periods_total counter
container_cpu_cfs_periods_total{container="app",namespace="shop",pod="web-abc"} %d %d
container_cpu_cfs_periods_total{container="",namespace="shop",pod="web-abc"} 9999 %d
container_cpu_cfs_periods_total{container="POD",namespace="shop",pod="web-abc"} 9999 %d
# HELP container_cpu_cfs_throttled_periods_total Number of throttled period intervals.
# TYPE container_cpu_cfs_throttled_periods_total counter
container_cpu_cfs_throttled_periods_total{container="app",namespace="shop",pod="web-abc"} %d %d
# HELP container_start_time_seconds Start time of the container since unix epoch in seconds.
# TYPE container_start_time_seconds gauge
container_start_time_seconds{container="app",namespace="shop",pod="web-abc"} %d
# HELP container_memory_working_set_bytes Current working set in bytes.
# TYPE container_memory_working_set_bytes gauge
container_memory_working_set_bytes{container="app",namespace="shop",pod="web-abc"} 12345 %d
`, periods, ms, ms, ms, throttled, ms, start.Unix(), ms)
}

func TestParseCadvisorKeepsOnlyAttributableThrottling(t *testing.T) {
	at := usageTestStart.Add(30 * time.Second)
	samples, err := parseCadvisor(strings.NewReader(fixture(40, 100, at, usageTestStart)))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 {
		t.Fatalf("parsed %d samples, want 1 (pod-level and pause rows must be dropped): %+v", len(samples), samples)
	}
	s := samples[cadvisorKey{namespace: "shop", pod: "web-abc", container: "app"}]
	if s == nil {
		t.Fatal("expected sample for shop/web-abc/app")
	}
	if !s.hasThrottled || !s.hasPeriods || s.throttled != 40 || s.periods != 100 {
		t.Errorf("sample counters = %+v, want throttled 40 / periods 100", s)
	}
	if !s.ts.Equal(at) {
		t.Errorf("sample time = %v, want the exposition timestamp %v", s.ts, at)
	}
	if !s.start.Equal(usageTestStart) {
		t.Errorf("start = %v, want %v", s.start, usageTestStart)
	}
}

func TestThrottlingFirstObservationCountsFromContainerStart(t *testing.T) {
	p := testPoller(webResolver())
	at := usageTestStart.Add(30 * time.Second)

	samples, err := parseCadvisor(strings.NewReader(fixture(40, 100, at, usageTestStart)))
	if err != nil {
		t.Fatal(err)
	}
	p.ingestCadvisor(samples, at)

	r := onlyRecord(t, p)
	if r.CPU.ThrottledPeriods != 40 || r.CPU.TotalPeriods != 100 {
		t.Errorf("throttled/total = %d/%d, want 40/100 attributed from container start",
			r.CPU.ThrottledPeriods, r.CPU.TotalPeriods)
	}
	if got := p.Signals(); len(got) != 1 || got[0] != "throttling" {
		t.Errorf("signals = %v, want [throttling]", got)
	}
}

func TestThrottlingDeltaRules(t *testing.T) {
	p := testPoller(webResolver())
	first := usageTestStart.Add(30 * time.Second)
	second := first.Add(30 * time.Second)
	third := second.Add(30 * time.Second)

	ingest := func(throttled, periods uint64, at time.Time, now time.Time) {
		t.Helper()
		samples, err := parseCadvisor(strings.NewReader(fixture(throttled, periods, at, usageTestStart)))
		if err != nil {
			t.Fatal(err)
		}
		p.ingestCadvisor(samples, now)
	}

	ingest(40, 100, first, first)
	// Re-served scrape: unchanged timestamp, no new information.
	ingest(40, 100, first, second)
	// Progress: +20 throttled, +100 periods.
	ingest(60, 200, second, second)
	// Counter reset (restart): rebaseline, emit nothing.
	ingest(5, 10, third, third)

	r := onlyRecord(t, p)
	if r.CPU.ThrottledPeriods != 60 || r.CPU.TotalPeriods != 200 {
		t.Errorf("throttled/total = %d/%d, want 60/200 (40+20 / 100+100, reset emits nothing)",
			r.CPU.ThrottledPeriods, r.CPU.TotalPeriods)
	}
}

// The cAdvisor exposition labels by pod name, and a name is not an identity: a
// StatefulSet recreates its pod under the same name with a new UID. Keying the
// counter baseline on the name made the two pods share one baseline, so the
// replacement's counters were compared against a dead container's — either
// silently dropped as a "counter reset", or, when the new counters happened to
// exceed the old baseline, attributed as one continuous run. Keyed on the UID,
// each container is its own series and the replacement is attributed from its
// own start, exactly as any first observation is.
func TestThrottlingDoesNotCrossPodRecreation(t *testing.T) {
	resolver := stubResolver{
		"uid-old": {namespace: "shop", name: "web-abc", workload: WorkloadRef{Kind: "StatefulSet", Name: "web"}},
	}
	p := testPoller(resolver)
	first := usageTestStart.Add(30 * time.Second)
	second := first.Add(30 * time.Second)
	newPodStart := second.Add(-10 * time.Second)

	ingest := func(throttled, periods uint64, at, start, now time.Time) {
		t.Helper()
		samples, err := parseCadvisor(strings.NewReader(fixture(throttled, periods, at, start)))
		if err != nil {
			t.Fatal(err)
		}
		p.ingestCadvisor(samples, now)
	}

	// The original pod runs and accumulates a large baseline.
	ingest(400, 1000, first, usageTestStart, first)

	// It is replaced: same namespace and name, new UID, a container that has
	// been up for ten seconds with counters of its own.
	delete(resolver, "uid-old")
	resolver["uid-new"] = podIndexEntry{
		namespace: "shop", name: "web-abc",
		workload: WorkloadRef{Kind: "StatefulSet", Name: "web"},
	}
	ingest(3, 10, second, newPodStart, second)

	r := onlyRecord(t, p)
	// 400/1000 from the first container plus the replacement's own 3/10 —
	// neither dropped as a phantom reset nor stretched into a delta against a
	// baseline that belonged to a different container.
	if r.CPU.ThrottledPeriods != 403 || r.CPU.TotalPeriods != 1010 {
		t.Errorf("throttled/total = %d/%d, want 403/1010 — each container attributed from its own start",
			r.CPU.ThrottledPeriods, r.CPU.TotalPeriods)
	}
	if len(p.throttle) != 2 {
		t.Errorf("throttle state entries = %d, want 2 (one series per pod UID)", len(p.throttle))
	}
}

// A container observed and quiet must be distinguishable from one never
// observed at all — the whole reason the sample count exists.
func TestThrottlingSampleCountSeparatesQuietFromUnobserved(t *testing.T) {
	p := testPoller(webResolver())
	at := usageTestStart.Add(30 * time.Second)
	samples, err := parseCadvisor(strings.NewReader(fixture(0, 100, at, usageTestStart)))
	if err != nil {
		t.Fatal(err)
	}
	p.ingestCadvisor(samples, at)

	r := onlyRecord(t, p)
	if r.CPU.ThrottledPeriods != 0 {
		t.Fatalf("throttled periods = %d, want 0", r.CPU.ThrottledPeriods)
	}
	if r.CPU.ThrottlingSamples == 0 {
		t.Error("throttling samples = 0 after a successful observation: a quiet container is now indistinguishable from an unobserved one")
	}
}

func TestThrottlingUnknownPodSkipped(t *testing.T) {
	p := testPoller(stubResolver{})
	at := usageTestStart.Add(30 * time.Second)
	samples, err := parseCadvisor(strings.NewReader(fixture(40, 100, at, usageTestStart)))
	if err != nil {
		t.Fatal(err)
	}
	p.ingestCadvisor(samples, at)
	if got := len(p.acc.Snapshots()); got != 0 {
		t.Fatalf("unattributable throttling produced %d records, want 0", got)
	}
	if got := len(p.throttle); got != 0 {
		t.Fatalf("unattributable throttling left %d tracker entries, want 0", got)
	}
}

func TestThrottlingSweepForgetsGoneContainers(t *testing.T) {
	p := testPoller(webResolver())
	at := usageTestStart.Add(30 * time.Second)
	samples, err := parseCadvisor(strings.NewReader(fixture(40, 100, at, usageTestStart)))
	if err != nil {
		t.Fatal(err)
	}
	p.ingestCadvisor(samples, at)
	if len(p.throttle) != 1 {
		t.Fatalf("throttle tracker has %d entries, want 1", len(p.throttle))
	}
	p.sweep(at.Add(staleTrackerAfter + time.Second))
	if len(p.throttle) != 0 {
		t.Fatal("sweep kept a throttle entry past the stale cutoff")
	}
}

func TestThrottlingMergesWithSummaryRecords(t *testing.T) {
	// Both endpoints feed the same rollup key: summary CPU/memory and
	// cAdvisor throttling must land in one record.
	p := testPoller(webResolver())
	at := usageTestStart.Add(30 * time.Second)

	p.ingest(summaryWith(cpuMemStats(usageTestStart, at, 15e9, 64<<20)), at)
	samples, err := parseCadvisor(strings.NewReader(fixture(40, 100, at, usageTestStart)))
	if err != nil {
		t.Fatal(err)
	}
	p.ingestCadvisor(samples, at)

	r := onlyRecord(t, p)
	if r.CPU.CoreNanoseconds != 15e9 || r.CPU.ThrottledPeriods != 40 {
		t.Errorf("core/throttled = %d/%d, want 15e9/40 in the same record",
			r.CPU.CoreNanoseconds, r.CPU.ThrottledPeriods)
	}
	if r.Key != (rollup.Key{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"}) {
		t.Errorf("record key = %+v", r.Key)
	}
}
