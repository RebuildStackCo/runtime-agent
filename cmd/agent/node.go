package main

import (
	"context"
	"flag"
	"log/slog"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// runNode is the node role's lifecycle. It scans on-node processes for Go
// build information and writes the result to the structured log. It builds NO
// Kubernetes client and opens no external connection: the DaemonSet's
// ServiceAccount holds zero RBAC and everything the scanner reads is under
// /proc (ADR 0009). Delivery of these findings to the controller is a later
// slice; here the output is the log line.
func runNode(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet(roleNode, flag.ExitOnError)
	procRoot := fs.String("proc", "/proc", "path to the proc filesystem to scan")
	interval := fs.Duration("interval", time.Minute, "scan interval; 0 runs a single pass and exits")
	if err := fs.Parse(args); err != nil {
		return err
	}

	scanner := nodescan.NewScanner(*procRoot, nodescan.NewModuleFilter(nodescan.DefaultInfraModulePrefixes))
	logger.Info("node scanner starting",
		"version", version,
		"proc_root", *procRoot,
		"interval", interval.String(),
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
