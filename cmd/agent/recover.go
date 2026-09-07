package main

import (
	"log/slog"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

// seedOpenWindows hands the poller whatever the spool still holds for windows
// that are open now, so a restart resumes them instead of overwriting them with
// an empty one (ADR 0072).
//
// Nothing here is fatal. A spool that cannot be read, a file that cannot be
// decoded and a record the accumulator refuses all leave the agent where it was
// before this existed: collecting, from nothing.
func seedOpenWindows(logger *slog.Logger, spool *sink.Spool, poller *collector.UsagePoller, now time.Time) {
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
}
