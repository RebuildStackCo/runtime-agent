package main

import (
	"log/slog"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/journal"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

// journals is the four accumulators a restart resumes beside the usage poller.
type journals struct {
	restarts    *journal.Restarts
	disruptions *journal.Disruptions
	nodes       *journal.NodeEvents
	jobs        *journal.JobRuns
}

// seedOpenWindows hands the poller and the journals whatever the spool still
// holds for windows that are open now, so a restart resumes them instead of
// overwriting them with a subset of themselves (ADR 0072, ADR 0077).
//
// Nothing here is fatal. A spool that cannot be read, a file that cannot be
// decoded and a record the accumulator refuses all leave the agent where it was
// before this existed: collecting, from nothing.
func seedOpenWindows(logger *slog.Logger, spool *sink.Spool, poller *collector.UsagePoller, j journals, now time.Time) {
	recovery, err := spool.RecoverOpenWindows(now)
	if err != nil {
		logger.Error("reading the spool for open windows", "error", err)
		return
	}
	for _, skipped := range recovery.Skipped {
		logger.Warn("a spool payload was left where it is", "error", skipped)
	}
	if err := poller.Seed(recovery.Records); err != nil {
		logger.Warn("some recovered records were not adopted", "error", err)
	}
	if recovery.Windows > 0 {
		logger.Info("resumed open usage windows from the spool",
			"windows", recovery.Windows, "records", len(recovery.Records))
	}

	seeded := j.restarts.Resume(recovery.Restarts) +
		j.disruptions.Resume(recovery.Disruptions) +
		j.nodes.Resume(recovery.NodeEvents) +
		j.jobs.Resume(recovery.JobRuns)
	if recovery.JournalWindows > 0 {
		logger.Info("resumed open journal windows from the spool",
			"windows", recovery.JournalWindows, "records", seeded)
	}
}
