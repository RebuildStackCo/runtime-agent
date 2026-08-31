package nodescan

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The descriptor reader (ADR 0061 §3): how many file descriptors a process holds
// against how many it may. Both halves come from the same process at the same
// moment, so the headroom between them is a fact and not a subtraction across
// two readings (ADR 0043's discipline).
//
// Only the counts are taken. What each descriptor points at is a path, a socket
// or a pipe — an identity, and the reason this reads the directory rather than
// its entries.

// maxLimitsBytes bounds the limits read; the file is a fixed table of about
// sixteen rows.
const maxLimitsBytes = 16 << 10

// ProcessFiles is one process's descriptor usage and its soft limit.
type ProcessFiles struct {
	// Open is how many descriptors the process holds. Zero means the directory
	// could not be read — a live process always holds at least its standard
	// streams.
	Open int
	// Limit is the soft RLIMIT_NOFILE, which is the one that applies. An
	// unlimited limit is reported as zero rather than as a very large number
	// that would read as a real ceiling.
	Limit int64
}

// ReadProcessFiles counts a process's open descriptors and reads its soft
// descriptor limit. Either half missing yields that half as zero.
func ReadProcessFiles(procRoot string, pid int) ProcessFiles {
	return ProcessFiles{
		Open:  countOpenFiles(procRoot, pid),
		Limit: readFileLimit(procRoot, pid),
	}
}

// countOpenFiles counts the entries of /proc/<pid>/fd without following them.
// The count is bounded by maxProcessFDs, the same bound the socket reader uses:
// past it the answer is "many", and the exact number changes no finding.
func countOpenFiles(procRoot string, pid int) int {
	d, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "fd")) // #nosec G304 -- procRoot is an operator-set flag; pid is a live process
	if err != nil {
		return 0
	}
	defer func() { _ = d.Close() }()
	total := 0
	for {
		names, err := d.Readdirnames(512)
		total += len(names)
		if err != nil || total >= maxProcessFDs {
			break
		}
	}
	return min(total, maxProcessFDs)
}

// readFileLimit takes the soft limit from the "Max open files" row of
// /proc/<pid>/limits. The row's columns are soft, hard and unit.
func readFileLimit(procRoot string, pid int) int64 {
	f, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "limits")) // #nosec G304 -- procRoot is an operator-set flag; pid is a live process
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(io.LimitReader(f, maxLimitsBytes))
	for scanner.Scan() {
		line := scanner.Text()
		rest, ok := strings.CutPrefix(line, "Max open files")
		if !ok {
			continue
		}
		soft, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
		n, err := strconv.ParseInt(soft, 10, 64)
		if err != nil || n <= 0 {
			return 0 // "unlimited", or a column this parser does not assume
		}
		return n
	}
	return 0
}
