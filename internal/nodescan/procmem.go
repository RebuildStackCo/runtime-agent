package nodescan

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The shared-memory reader (ADR 0061 §2). `/proc/<pid>/smaps_rollup` is the
// kernel's own aggregate over every mapping, so this is one small file rather
// than a walk of `smaps`, which on a large address space is thousands of lines.
//
// It is the one read in this pass with a cost the others do not have: the kernel
// walks the page tables to compute Pss.

// maxSmapsBytes bounds the read. smaps_rollup is under a kilobyte on every
// kernel that has it; past this it is not the file this parser assumes.
const maxSmapsBytes = 16 << 10

// ProcessMemory is what the scanner keeps from one process's rollup.
type ProcessMemory struct {
	// PSSBytes divides each shared page among the processes mapping it, so
	// summing it over the replicas of one image gives the memory they actually
	// occupy rather than the same binary counted once per replica.
	PSSBytes int64
	// PrivateDirtyBytes is the memory that belongs to this process alone and
	// cannot be dropped — the true marginal cost of one more replica.
	PrivateDirtyBytes int64
}

// ReadProcessMemory reads one process's smaps_rollup. A missing file yields the
// zero value: the kernel predates the file (before 4.14), or the process exited,
// and neither is worth a counter — the record's process count already says how
// many processes stood behind the numbers.
func ReadProcessMemory(procRoot string, pid int) ProcessMemory {
	f, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "smaps_rollup")) // #nosec G304 -- procRoot is an operator-set flag; pid is a live process
	if err != nil {
		return ProcessMemory{}
	}
	defer func() { _ = f.Close() }()
	return parseProcessMemory(io.LimitReader(f, maxSmapsBytes))
}

// parseProcessMemory takes the two allow-listed keys. The file's first line is
// the address range and the mapping's permissions, which this ignores: it names
// no key, and a mapping's path is an identity the agent does not collect.
func parseProcessMemory(r io.Reader) ProcessMemory {
	var out ProcessMemory
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Pss":
			out.PSSBytes = peakBytes(value)
		case "Private_Dirty":
			out.PrivateDirtyBytes = peakBytes(value)
		}
	}
	return out
}
