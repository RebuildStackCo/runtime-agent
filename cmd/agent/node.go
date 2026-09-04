package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/config"
	"github.com/RebuildStackCo/runtime-agent/internal/ebpfgate"
	"github.com/RebuildStackCo/runtime-agent/internal/health"
	"github.com/RebuildStackCo/runtime-agent/internal/nodeprofile"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// defaultControllerTokenPath is where the DaemonSet's projected
// serviceAccountToken volume mounts the controller-audience token (ADR 0010).
// It is a mount path, not a secret value.
const defaultControllerTokenPath = "/var/run/secrets/rebuildstack.co/controller-token/token" // #nosec G101 -- filesystem path, not an embedded credential

// nodePassCeiling is what one scan pass may spend on the controller: the scope
// query and the report delivery, each bounded by its client's own 30s timeout
// (nodeship.go). Liveness allows three intervals plus this, so a node whose
// controller is unreachable is late but never dead (ADR 0069).
//
// A variable rather than a constant only so a test can compress it; production
// never assigns it (see coverageInterval).
var nodePassCeiling = time.Minute

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
	healthAddress := fs.String("health-address", "", "address for the health listener (ADR 0069), e.g. \":9090\"; empty opens no listener")
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

	// The node this agent speaks for; the DaemonSet sets NODE_NAME via the
	// downward API, from the same `spec.nodeName` the kubelet writes into the
	// projected token. The controller now requires the two to agree (ADR 0040),
	// so an empty value is no longer harmless — every request would be refused
	// with 400 and the node would scan nothing while looking healthy. Fail here
	// instead, where the message names the missing variable.
	node := os.Getenv("NODE_NAME")
	if node == "" {
		return fmt.Errorf("NODE_NAME is not set: the controller matches it against the node named " +
			"in this pod's token, so a node that cannot name itself cannot report")
	}
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
	// holds what the profiler did, for the node's log and for its report.
	ebpfMetrics := newProfilingMetrics()
	// What the scanner learns about whose code each container runs, for the
	// profiler beside it. Published on every pass and read per window; the two
	// are different goroutines in this one process (ADR 0059 §1).
	moduleIndex := &nodeprofile.ModuleIndex{}
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
				targetsCl, newScopeClient(*scopeEndpoint, *tokenPath), profileSh,
				ebpfMetrics, moduleIndex)
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

	// What the two probes read. The heartbeat is stamped at the top of a pass,
	// so a pass wedged in a syscall goes stale; `passed` latches when the first
	// one finishes, which is what makes a DaemonSet rollout wait for a node that
	// has actually scanned (ADR 0069).
	beat := health.NewHeartbeat(time.Now(), nodeLivenessDeadline(*interval))
	var passed atomic.Bool

	scanOnce := func() {
		beat.Beat(time.Now())
		defer passed.Store(true)
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
		// The pass's own answer to whose code each container runs, replacing the
		// last one wholesale so a container no live process belongs to leaves
		// with it (ADR 0059 §1).
		byContainer := map[string][]string{}
		for _, b := range res.Binaries {
			if b.ContainerID == "" {
				continue
			}
			byContainer[b.ContainerID] = append(byContainer[b.ContainerID],
				nodescan.OwnModules(b.MainModule, b.Dependencies)...)
		}
		moduleIndex.Publish(byContainer)

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
			"containers_with_known_modules", moduleIndex.Size(),
		)

		// What the profiler beside this scanner did, on the scanner's cadence
		// because it is the only one that fires when the profiler does not
		// (ADR 0060 §1). Reported always, logged only where it was asked for:
		// "disabled" is worth shipping and not worth a line every pass.
		prof := ebpfMetrics.snapshot()
		if *enableEBPF {
			logger.Info("ebpf coverage",
				"state", prof.State,
				"windows", prof.Windows,
				"no_scope", prof.WindowsNoScope,
				"no_targets", prof.WindowsNoTargets,
				"no_samples", prof.WindowsNoSamples,
				"shipped", prof.ProfilesShipped,
				"invalid", prof.ProfilesInvalid,
				"unshipped", prof.ProfilesUnshipped,
			)
		}

		// Ship to the controller when configured. Best-effort: a delivery
		// failure is logged and the next pass retries — the controller
		// rebuilds inventory from re-scans, so a lost report costs nothing
		// (ADR 0010).
		if shipper != nil {
			if err := shipper.ship(ctx, res, prof); err != nil {
				logger.Error("shipping report to controller failed", "error", err)
			} else {
				logger.Info("report shipped to controller",
					"go_found", res.Counters.GoFound,
					"endpoint", *endpoint,
				)
			}
		}
	}

	// The health listener, when the installation configured one. Liveness is the
	// scan loop's own stamp and nothing else: the controller being unreachable
	// makes a node late, never dead, for the same reason the controller's own
	// liveness ignores the API server (ADR 0069).
	healthErr := make(chan error, 1)
	if *healthAddress != "" {
		live := func() (bool, string) {
			if alive, age := beat.Alive(time.Now()); !alive {
				return false, fmt.Sprintf("the scan pass last started %s ago", age.Round(time.Second))
			}
			return true, ""
		}
		ready := func() (bool, string) {
			if !passed.Load() {
				return false, "the first scan pass has not finished"
			}
			return true, ""
		}
		healthCtx, stopHealth := context.WithCancel(ctx)
		defer stopHealth()
		srv := health.New(*healthAddress, live, ready, logger)
		go func() { healthErr <- srv.Run(healthCtx) }()
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
		case err := <-healthErr:
			// The listener stopped while the scanner did not: it could not bind,
			// or serving failed. A node whose probes cannot be answered is
			// restarted by the kubelet regardless, so exit with the reason.
			return err
		case <-ticker.C:
			scanOnce()
		}
	}
}

// nodeLivenessDeadline is how stale the scan loop's stamp may get: three missed
// passes plus what one pass may spend waiting on the controller.
func nodeLivenessDeadline(interval time.Duration) time.Duration {
	if interval <= 0 {
		return nodePassCeiling
	}
	return 3*interval + nodePassCeiling
}

// ebpfGate evaluates the eBPF readiness gate once, logs a clear line, records
// the outcome on m as this node's profiling state, and returns it. A refusal is
// deliberately non-fatal: the node keeps running the Go-binary scanner rather
// than escalating to privileged (ADR 0011 §2). It logs only the kernel version
// and BTF fact — never anything derived from a customer workload.
//
// The reason is low-cardinality because it now leaves the node (ADR 0060 §2).
func ebpfGate(logger *slog.Logger, procRoot, sysRoot string, m *profilingMetrics) ebpfgate.Result {
	res := ebpfgate.Probe(procRoot, sysRoot)
	m.setState(string(res.Reason))
	if res.Supported() {
		logger.Info("ebpf profile ready",
			"kernel", res.KernelVersion,
			"btf", res.BTFPresent,
		)
		return res
	}
	logger.Warn("ebpf profile refused; continuing as scanner",
		"reason", string(res.Reason),
		"kernel", res.KernelVersion,
		"btf", res.BTFPresent,
	)
	return res
}
