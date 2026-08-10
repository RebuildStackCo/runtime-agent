package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

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
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The node identity for the report; the DaemonSet sets NODE_NAME via the
	// downward API. Empty is acceptable (the controller keys facts by pod UID
	// and container ID, not node name).
	node := os.Getenv("NODE_NAME")
	shipper := newReportShipper(*endpoint, *tokenPath, node)

	scanner := nodescan.NewScanner(*procRoot, nodescan.NewModuleFilter(nodescan.DefaultInfraModulePrefixes))
	logger.Info("node scanner starting",
		"version", version,
		"proc_root", *procRoot,
		"interval", interval.String(),
		"controller_endpoint", *endpoint,
		"node", node,
	)
	defer logger.Info("node scanner stopping")

	scanOnce := func() {
		res, err := scanner.Scan()
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
			"go_found", res.Counters.GoFound,
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
