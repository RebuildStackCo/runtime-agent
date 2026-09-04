package main

import (
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
)

// The container memory limit, passed in by the chart through the downward API
// because the Go runtime does not read it (it reads the CPU limit and sets
// GOMAXPROCS; there is no memory equivalent). The variable is not named
// GOMEMLIMIT: it carries the whole limit, which the runtime would take as the
// heap ceiling itself (ADR 0068 §5).
const memoryLimitEnv = "MEMORY_LIMIT_BYTES"

// What of the container's limit the Go heap may have. The rest is the reserve
// for everything GOMEMLIMIT does not account for — 61.5 MiB of demand-paged
// text, rodata and pclntab in this binary, measured (ADR 0068 §5).
const memoryLimitFraction = 0.80

// applyMemoryLimit sets the runtime's soft memory limit from the environment,
// and says what it did. Without it an over-budget controller is OOM-killed:
// SIGKILL, so no shutdown flush pass and up to one coverage interval of the
// journals gone.
func applyMemoryLimit(logger *slog.Logger) {
	raw, ok := os.LookupEnv(memoryLimitEnv)
	if !ok {
		return // no limit rendered; the Go default stands (ADR 0068 §5)
	}
	limit, err := memoryLimit(raw)
	if err != nil {
		logger.Warn("ignoring the container memory limit; running on the Go default",
			"env", memoryLimitEnv, "value", raw, "error", err)
		return
	}
	debug.SetMemoryLimit(limit)
	logger.Info("memory limit applied",
		"container_limit_bytes", raw,
		"go_memory_limit_bytes", limit,
		"fraction", memoryLimitFraction,
	)
}

// memoryLimit is the heap ceiling for a container limit of raw bytes.
func memoryLimit(raw string) (int64, error) {
	bytes, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	if bytes <= 0 {
		return 0, strconv.ErrRange
	}
	return int64(float64(bytes) * memoryLimitFraction), nil
}
