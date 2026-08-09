package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
)

// The only metric families the agent reads from /metrics/cadvisor: the CFS
// throttling counters (ADR 0006) and the container start time that anchors a
// first observation. Everything else in the multi-megabyte exposition is
// decoded family-by-family and discarded immediately.
const (
	metricThrottledPeriods = "container_cpu_cfs_throttled_periods_total"
	metricTotalPeriods     = "container_cpu_cfs_periods_total"
	metricStartTime        = "container_start_time_seconds"
)

// cadvisorKey identifies a container in the cAdvisor exposition, which
// labels by name, not UID.
type cadvisorKey struct {
	namespace string
	pod       string
	container string
}

// cadvisorSample is one container's throttling reading from a single scrape.
type cadvisorSample struct {
	throttled    uint64
	periods      uint64
	hasThrottled bool
	hasPeriods   bool
	ts           time.Time
	start        time.Time
}

// throttleState is the per-container counter baseline for the CFS counters,
// in-memory only — loss-harmless like every baseline.
type throttleState struct {
	ts        time.Time
	throttled uint64
	periods   uint64
	lastSeen  time.Time
}

// fetchCadvisor reads one kubelet's /metrics/cadvisor through the API server
// proxy — the second of the exactly two stats paths the agent ever requests
// via nodes/proxy (docs/security.md §4) — and returns the throttling samples.
func (p *UsagePoller) fetchCadvisor(ctx context.Context, node string) (map[cadvisorKey]*cadvisorSample, error) {
	stream, err := p.clientset.CoreV1().RESTClient().Get().
		Resource("nodes").Name(node).SubResource("proxy").
		Suffix("metrics/cadvisor").Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching cadvisor metrics: %w", err)
	}
	defer func() { _ = stream.Close() }()
	samples, err := parseCadvisor(stream)
	if err != nil {
		return nil, fmt.Errorf("decoding cadvisor metrics: %w", err)
	}
	return samples, nil
}

// parseCadvisor decodes a cAdvisor text exposition one metric family at a
// time, keeping only the throttling counters and container start times.
// Pod-level rows (empty container label) and pause containers ("POD") are
// dropped here — they attribute to no workload container.
func parseCadvisor(r io.Reader) (map[cadvisorKey]*cadvisorSample, error) {
	samples := make(map[cadvisorKey]*cadvisorSample)
	dec := expfmt.NewDecoder(r, expfmt.NewFormat(expfmt.TypeTextPlain))
	for {
		var family dto.MetricFamily
		if err := dec.Decode(&family); err != nil {
			if errors.Is(err, io.EOF) {
				return samples, nil
			}
			return nil, err
		}
		name := family.GetName()
		if name != metricThrottledPeriods && name != metricTotalPeriods && name != metricStartTime {
			continue
		}
		for _, m := range family.GetMetric() {
			key, ok := cadvisorMetricKey(m)
			if !ok {
				continue
			}
			sample := samples[key]
			if sample == nil {
				sample = &cadvisorSample{}
				samples[key] = sample
			}
			switch name {
			case metricThrottledPeriods:
				sample.throttled, sample.hasThrottled = counterValue(m)
				sample.ts = metricTime(m, sample.ts)
			case metricTotalPeriods:
				sample.periods, sample.hasPeriods = counterValue(m)
				sample.ts = metricTime(m, sample.ts)
			case metricStartTime:
				if v := m.GetGauge().GetValue(); v > 0 {
					sample.start = time.Unix(0, int64(v*float64(time.Second)))
				}
			}
		}
	}
}

func cadvisorMetricKey(m *dto.Metric) (cadvisorKey, bool) {
	var key cadvisorKey
	for _, label := range m.GetLabel() {
		switch label.GetName() {
		case "namespace":
			key.namespace = label.GetValue()
		case "pod":
			key.pod = label.GetValue()
		case "container":
			key.container = label.GetValue()
		}
	}
	if key.namespace == "" || key.pod == "" || key.container == "" || key.container == "POD" {
		return cadvisorKey{}, false
	}
	return key, true
}

// counterValue reads a counter sample, rejecting NaN and negatives.
func counterValue(m *dto.Metric) (uint64, bool) {
	v := m.GetCounter().GetValue()
	if math.IsNaN(v) || v < 0 {
		return 0, false
	}
	return uint64(v), true
}

// metricTime prefers the exposition's own timestamp — deltas are computed
// from response timestamps, never scrape time (ADR 0006) — falling back to
// whatever the sibling counter already recorded.
func metricTime(m *dto.Metric, fallback time.Time) time.Time {
	if ms := m.GetTimestampMs(); ms != 0 {
		return time.UnixMilli(ms)
	}
	return fallback
}

// ingestCadvisor applies the throttling delta rules — the same rules as
// every cumulative counter: response timestamps only, unchanged timestamp
// discarded, counter running backwards rebaselines silently, first
// observation counts from container start when the exposition provides it.
func (p *UsagePoller) ingestCadvisor(samples map[cadvisorKey]*cadvisorSample, now time.Time) {
	for key, sample := range samples {
		if !sample.hasThrottled || !sample.hasPeriods {
			continue
		}
		workload, ok := p.pods.LookupPodByName(key.namespace, key.pod)
		if !ok {
			continue
		}
		p.markSignal("throttling")
		at := sample.ts
		if at.IsZero() {
			at = now // exposition without timestamps: scrape time is all there is
		}

		rk := rollup.Key{
			Namespace:    key.namespace,
			WorkloadKind: workload.Kind,
			WorkloadName: workload.Name,
			Container:    key.container,
		}
		st := p.throttle[key]
		if st == nil {
			st = &throttleState{}
			p.throttle[key] = st
		}
		st.lastSeen = now

		switch {
		case st.ts.IsZero():
			// First observation: attributable from container start if the
			// exposition carries it; otherwise it can only seed the baseline.
			if !sample.start.IsZero() && at.After(sample.start) {
				p.acc.ObserveThrottling(rk, sample.start, at,
					clampToInt64(sample.throttled), clampToInt64(sample.periods))
			}
		case !at.After(st.ts):
			continue // re-served scrape — baseline stays
		case sample.throttled < st.throttled || sample.periods < st.periods:
			// Container restarted: rebaseline, emit nothing.
		default:
			p.acc.ObserveThrottling(rk, st.ts, at,
				clampToInt64(sample.throttled-st.throttled), clampToInt64(sample.periods-st.periods))
		}
		st.ts, st.throttled, st.periods = at, sample.throttled, sample.periods
	}
}
