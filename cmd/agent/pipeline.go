package main

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/config"
	"github.com/RebuildStackCo/runtime-agent/internal/ebpfgate"
	"github.com/RebuildStackCo/runtime-agent/internal/nodeintake"
	"github.com/RebuildStackCo/runtime-agent/internal/nodeprofile"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// samplesPerSecond maps the overhead ceiling (a percent) to a system-wide
// sampling rate. The eBPF profiler samples continuously and cannot be cheaply
// paused, so the ceiling is honored as a rate cap, not a duty cycle (ADR 0011,
// slice 6c): overhead scales with the rate. The mapping is a deliberately simple,
// documented heuristic — real overhead depends on the machine — bounded to a
// sane range.
func samplesPerSecond(overheadCeilingPercent int) int {
	sps := overheadCeilingPercent * 4 // ceiling 5% -> 20 Hz (the default)
	if sps < 1 {
		sps = 1
	}
	if sps > 99 {
		sps = 99
	}
	return sps
}

// runProfilingPipeline runs the eBPF profiler and, on a cadence, cuts a window,
// asks the controller which containers to profile on this node, groups the
// window's samples by container, filters symbols on the node, serializes,
// validates, and ships each profile. A capture that cannot load degrades to
// scanner-only, recorded as program_load_failed (ADR 0011 §2). Blocks until ctx
// is cancelled.
//
// Two controller answers bound a window, and they are refreshed on different
// cadences because they answer different questions. The target set — which
// containers rank highest — changes with consumption and is refetched every
// IntervalSeconds. The scan scope — which pods passed the customer's filters —
// changes with the cluster and is refetched every window, matching the
// scanner's own cadence (ADR 0015). Both must admit a container before its
// samples are shipped, and the scope fails closed exactly as the scanner does:
// no answer, nothing shipped (ADR 0025).
func runProfilingPipeline(ctx context.Context, logger *slog.Logger, p config.NodeProfiling,
	procRoot, node string, targets *targetsClient, scoper *scopeClient,
	shipper *profileShipper, m *ebpfGateMetrics) {

	sps := samplesPerSecond(p.OverheadCeilingPercent)
	session, err := nodeprofile.Start(ctx, logger, nodeprofile.Config{SamplesPerSecond: sps})
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			m.refusals[ebpfgate.ReasonProgramLoadFailed]++
			logger.Warn("ebpf capture unavailable; continuing as scanner",
				"reason", string(ebpfgate.ReasonProgramLoadFailed), "error", err)
		}
		return
	}

	thirdParty := nodeprofile.ThirdPartyDrop
	if p.ThirdPartySymbols == config.ThirdPartySymbolsKeep {
		thirdParty = nodeprofile.ThirdPartyKeep
	}
	filter := nodeprofile.NewSymbolFilter(p.AllowedModulePrefixes, thirdParty)

	window := time.Duration(p.CaptureDurationSeconds) * time.Second // ship cadence / window length
	refresh := time.Duration(p.IntervalSeconds) * time.Second       // targets-refresh cadence
	ticker := time.NewTicker(window)
	defer ticker.Stop()

	var (
		targetSet map[string]struct{}
		lastFetch time.Time
		cursor    int // rotation cursor across windows
		start     = time.Now()
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			end := time.Now()
			if targets != nil && (targetSet == nil || end.Sub(lastFetch) >= refresh) {
				if ids, err := targets.fetch(ctx, node); err != nil {
					logger.Warn("targets query failed; keeping previous set", "error", err)
				} else {
					targetSet = toStringSet(ids)
					lastFetch = end
				}
			}
			// Fail closed, like the scanner: an unreachable controller or an
			// unconfigured endpoint costs this window's captures, never a
			// widening of what is profiled. The samples are dropped with the
			// window; nothing accumulates waiting for a scope.
			scope := nodescan.DenyAll()
			if scoper == nil {
				logger.Warn("no scope endpoint configured; profiling nothing",
					"hint", "set -scope-endpoint so the controller can supply the pods this node may profile")
			} else if uids, err := scoper.fetch(ctx, node); err != nil {
				logger.Error("fetching scope failed; profiling nothing this window", "error", err)
			} else {
				scope = nodescan.NewScope(uids)
			}
			cursor = processWindow(ctx, logger, filter, sps, procRoot, node,
				start, end, session.Drain(), targetSet, scope, p.MaxTargetsPerWindow, cursor, shipper)
			start = time.Now()
		}
	}
}

// processWindow groups a window's samples by container, keeps only containers
// that both the controller targeted and the scope admits, and ships up to
// maxPerWindow of them, rotating the start point by cursor so no target
// monopolizes capture. It returns the next cursor.
//
// The two conditions are not redundant. The target set says which containers are
// worth profiling; the scope says which pods the customer's filters admit at
// all. A container in the first and not the second is one whose executable the
// scanner is forbidden to open (ADR 0015) — profiling it would take the more
// sensitive signal from the pod denied the less sensitive one.
func processWindow(ctx context.Context, logger *slog.Logger, filter *nodeprofile.SymbolFilter,
	sps int, procRoot, node string, start, end time.Time, samples []nodeprofile.Sample,
	targetSet map[string]struct{}, scope nodescan.Scope, maxPerWindow, cursor int,
	shipper *profileShipper) int {

	if len(samples) == 0 || len(targetSet) == 0 || scope.Size() == 0 {
		return cursor
	}

	type group struct {
		podUID  string
		samples []nodeprofile.Sample
	}
	groups := map[string]*group{}
	bindings := map[int64]nodescan.PodBinding{}
	outOfScope := 0
	for _, s := range samples {
		b, ok := bindings[s.PID]
		if !ok {
			b = nodescan.ReadBinding(procRoot, int(s.PID))
			bindings[s.PID] = b
		}
		if b.ContainerID == "" {
			continue
		}
		if _, want := targetSet[b.ContainerID]; !want {
			continue
		}
		if !scope.Admits(b.PodUID) {
			// Counted, never identified: the pod UID of an out-of-scope sample
			// does not reach the log (CLAUDE.md invariant 6). A non-zero count
			// means the controller named a container its own filters exclude —
			// informer lag, or a controller disagreeing with itself.
			outOfScope++
			continue
		}
		g := groups[b.ContainerID]
		if g == nil {
			g = &group{podUID: b.PodUID}
			groups[b.ContainerID] = g
		}
		g.samples = append(g.samples, s)
	}
	if outOfScope > 0 {
		logger.Warn("dropped targeted samples outside the controller's own scan scope",
			"samples", outOfScope)
	}
	if len(groups) == 0 {
		return cursor
	}

	// Deterministic rotation over the targeted containers this window.
	cids := make([]string, 0, len(groups))
	for cid := range groups {
		cids = append(cids, cid)
	}
	sort.Strings(cids)
	if maxPerWindow <= 0 || maxPerWindow > len(cids) {
		maxPerWindow = len(cids)
	}
	cursor %= len(cids)
	for i := 0; i < maxPerWindow; i++ {
		cid := cids[(cursor+i)%len(cids)]
		g := groups[cid]

		filtered, _ := filter.Filter(g.samples)
		pprof, err := nodeprofile.Serialize(filtered, sps)
		if err != nil {
			logger.Error("serializing profile failed", "error", err)
			continue
		}
		if err := nodeprofile.Validate(pprof); err != nil {
			logger.Info("profile invalid; not shipped", "reason", err.Error())
			continue
		}
		if shipper != nil {
			report := nodeintake.ProfileReport{
				Node: node, PodUID: g.podUID, ContainerID: cid,
				CaptureStart: start, CaptureEnd: end, Pprof: pprof,
			}
			if err := shipper.ship(ctx, report); err != nil {
				logger.Warn("shipping profile failed", "error", err)
			}
		}
	}
	return cursor + maxPerWindow
}

func toStringSet(xs []string) map[string]struct{} {
	m := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		m[x] = struct{}{}
	}
	return m
}
