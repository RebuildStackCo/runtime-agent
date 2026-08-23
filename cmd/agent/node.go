package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/config"
	"github.com/RebuildStackCo/runtime-agent/internal/ebpfgate"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// defaultControllerTokenPath is where the DaemonSet's projected
// serviceAccountToken volume mounts the controller-audience token (ADR 0010).
// It is a mount path, not a secret value.
const defaultControllerTokenPath = "/var/run/secrets/rebuildstack.co/controller-token/token" // #nosec G101 -- filesystem path, not an embedded credential

// runNode is the node role's lifecycle. It scans on-node processes for Go
// build information, writes the result to the structured log, and — when a
// controller endpoint is configured — ships the result to the controller over
// HTTP, authenticated by a projected ServiceAccount token (ADR 0010). It builds
// NO Kubernetes client: the DaemonSet's ServiceAccount holds zero RBAC and the
// projected token is audience-bound to the controller, so it cannot reach the
// API server (ADR 0009). Everything the scanner reads is under /proc. With no
// endpoint configured the output is the log line only, as in the first slice.
func runNode(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet(roleNode, flag.ExitOnError)
	procRoot := fs.String("proc", "/proc", "path to the proc filesystem to scan")
	interval := fs.Duration("interval", time.Minute, "scan interval; 0 runs a single pass and exits")
	endpoint := fs.String("controller-endpoint", "", "controller URL to POST scan reports to; empty runs log-only")
	tokenPath := fs.String("token-path", defaultControllerTokenPath, "path to the projected controller-audience ServiceAccount token")
	enableEBPF := fs.Bool("enable-ebpf", false, "master switch for the eBPF CPU profiler (ADR 0011); when set, the node checks eBPF readiness at startup and refuses gracefully if the kernel cannot support it")
	sysRoot := fs.String("sys", "/sys", "sysfs root used to check kernel BTF at <sys>/kernel/btf/vmlinux")
	configPath := fs.String("config", "", "path to the agent configuration file (YAML); supplies the eBPF profiling config (ADR 0011)")
	scopeEndpoint := fs.String("scope-endpoint", "", "controller URL to query for the pods this node may scan (ADR 0015); empty means the node scans nothing — it cannot honor namespace filters on its own")
	targetsEndpoint := fs.String("targets-endpoint", "", "controller URL to query for profiling targets (ADR 0011); empty disables targeting")
	profileEndpoint := fs.String("profile-endpoint", "", "controller URL to ship captured profiles to (ADR 0011); empty runs capture-only")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The node's configuration schema is narrower than the controller's, and
	// holds only what the node enforces on its own samples: the symbol
	// allow-list that decides what may leave this node, and the ceilings on what
	// profiling costs it. Which workloads get profiled is not here and not in
	// profiling at all — it is which workloads the controller collects
	// (ADR 0025). A controller-only setting placed in this file is a
	// startup error, not a field parsed and ignored. -enable-ebpf remains the
	// master switch; this config is additive.
	cfg, err := config.LoadNode(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	profiling := cfg.Profiling.Normalized()

	// The node identity for the report; the DaemonSet sets NODE_NAME via the
	// downward API. Empty is acceptable (the controller keys facts by pod UID
	// and container ID, not node name).
	node := os.Getenv("NODE_NAME")
	shipper := newReportShipper(*endpoint, *tokenPath, node)
	scoper := newScopeClient(*scopeEndpoint, *tokenPath)

	scanner := nodescan.NewScanner(*procRoot, nodescan.NewModuleFilter(nodescan.DefaultInfraModulePrefixes))
	logger.Info("node scanner starting",
		"version", version,
		"proc_root", *procRoot,
		"interval", interval.String(),
		"controller_endpoint", *endpoint,
		"node", node,
	)
	defer logger.Info("node scanner stopping")

	// eBPF readiness gate (ADR 0011). Evaluated once, before any profiling.
	// When the gate passes, the profiler runs alongside the scanner in its own
	// goroutine. A refusal — at the gate or later at eBPF program load —
	// degrades to scanner-only, never an escalation to privileged. ebpfMetrics
	// is the in-process counter, surfaced in the node's own log.
	ebpfMetrics := newEBPFGateMetrics()
	if *enableEBPF {
		logger.Info("ebpf profiling config",
			"allowed_module_prefixes", len(profiling.AllowedModulePrefixes),
			"third_party_symbols", profiling.ThirdPartySymbols,
			"capture_duration_s", profiling.CaptureDurationSeconds,
			"interval_s", profiling.IntervalSeconds,
			"overhead_ceiling_pct", profiling.OverheadCeilingPercent,
		)
		if res := ebpfGate(logger, *procRoot, *sysRoot, ebpfMetrics); res.Supported() {
			targetsCl := newTargetsClient(*targetsEndpoint, *tokenPath)
			profileSh := newProfileShipper(*profileEndpoint, *tokenPath)
			// The pipeline gets its own scope client: a captured sample is only
			// shipped if its pod is in the scope the controller supplies, the
			// same set that gates scanning (ADR 0015). Profiling a pod whose
			// executable the scanner is not allowed to open was an asymmetry,
			// and stack traces are the more sensitive of the two (ADR 0025).
			go runProfilingPipeline(ctx, logger, profiling, *procRoot, node,
				targetsCl, newScopeClient(*scopeEndpoint, *tokenPath), profileSh, ebpfMetrics)
		}
	}

	// scanScope asks the controller which pods on this node passed the
	// customer's namespace filters and opt-out annotations. It is fetched per
	// pass, and it fails closed: with no endpoint configured, or a controller
	// that cannot be reached, the node scans no process's executable at all
	// (ADR 0015). The node cannot decide eligibility itself — its cgroup gives
	// it a pod UID, never a namespace — so scanning without an answer would
	// break the promise in docs/security.md §10.2. A skipped pass costs
	// nothing: the next one re-scans (loss-harmless, ADR 0003).
	scanScope := func() nodescan.Scope {
		if scoper == nil {
			logger.Warn("no scope endpoint configured; scanning nothing",
				"hint", "set -scope-endpoint so the controller can supply the pods this node may scan")
			return nodescan.DenyAll()
		}
		uids, err := scoper.fetch(ctx, node)
		if err != nil {
			logger.Error("fetching scan scope failed; scanning nothing this pass", "error", err)
			return nodescan.DenyAll()
		}
		return nodescan.NewScope(uids)
	}

	scanOnce := func() {
		scope := scanScope()
		res, err := scanner.Scan(scope)
		if err != nil {
			// A failure to read the process tree at all (not an individual
			// unreadable binary). Log and try again next tick.
			logger.Error("scan failed", "error", err)
			return
		}
		// Full detail for kept (customer workload) binaries.
		for _, b := range res.Binaries {
			logger.Info("go binary detected",
				"pid", b.PID,
				"go_version", b.GoVersion,
				"main_module", b.MainModule,
				"pgo", b.PGO,
				"dependencies", len(b.Dependencies),
				"pod_uid", b.PodUID,
				"container_id", b.ContainerID,
			)
		}
		// Aggregate-only line for everything not kept: no identities of
		// filtered-out or unreadable binaries ever appear (CLAUDE.md
		// invariant 6, docs/security.md §8).
		logger.Info("scan coverage",
			"processes_scanned", res.Counters.ProcessesScanned,
			"pods_in_scope", scope.Size(),
			"go_found", res.Counters.GoFound,
			"filtered_scope", res.Counters.FilteredScope,
			"filtered_infra", res.Counters.FilteredInfra,
			"unreadable", res.Counters.Unreadable,
		)

		// Ship to the controller when configured. Best-effort: a delivery
		// failure is logged and the next pass retries — the controller
		// rebuilds inventory from re-scans, so a lost report costs nothing
		// (ADR 0010).
		if shipper != nil {
			if err := shipper.ship(ctx, res); err != nil {
				logger.Error("shipping report to controller failed", "error", err)
			} else {
				logger.Info("report shipped to controller",
					"go_found", res.Counters.GoFound,
					"endpoint", *endpoint,
				)
			}
		}
	}

	scanOnce()
	if *interval <= 0 {
		return nil
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scanOnce()
		}
	}
}

// ebpfGateMetrics counts eBPF readiness outcomes per reason. In this slice it is
// in-process only; a report consumer surfaces it to the controller when the
// profiler ships (ADR 0011). The map key is the low-cardinality refusal reason,
// so a fleet's counts show *why* nodes declined (e.g. kernel_too_old vs
// btf_absent), which is what we would need to know to support new kernels.
type ebpfGateMetrics struct {
	ready    int
	refusals map[ebpfgate.Reason]int
}

func newEBPFGateMetrics() *ebpfGateMetrics {
	return &ebpfGateMetrics{refusals: make(map[ebpfgate.Reason]int)}
}

// ebpfGate evaluates the eBPF readiness gate once, logs a clear line, records
// the outcome on m, and returns it. A refusal is deliberately non-fatal: the
// node keeps running the Go-binary scanner rather than escalating to privileged
// (ADR 0011 §2). It logs only the kernel version and BTF fact — never anything
// derived from a customer workload.
func ebpfGate(logger *slog.Logger, procRoot, sysRoot string, m *ebpfGateMetrics) ebpfgate.Result {
	res := ebpfgate.Probe(procRoot, sysRoot)
	if res.Supported() {
		m.ready++
		logger.Info("ebpf profile ready",
			"kernel", res.KernelVersion,
			"btf", res.BTFPresent,
		)
		return res
	}
	m.refusals[res.Reason]++
	logger.Warn("ebpf profile refused; continuing as scanner",
		"reason", string(res.Reason),
		"kernel", res.KernelVersion,
		"btf", res.BTFPresent,
	)
	return res
}
