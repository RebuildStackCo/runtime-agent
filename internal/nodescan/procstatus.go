package nodescan

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The process-status reader (ADR 0052). It is the second thing the scanner
// opens per process, and the first that is not about the build.
//
// `/proc/<pid>/status` is a mixed file: beside the two numbers below it holds
// `Name`, the executable's own basename, which is an identity the agent does not
// collect. So the keys are an allow-list, exactly as build settings are
// (ADR 0019) — a deny-list would have to keep pace with a format the kernel
// owns.

// maxStatusBytes bounds the read. The file is a few kilobytes on every kernel;
// past this it is not the file this parser assumes.
const maxStatusBytes = 64 << 10

// ProcessStatus is what the scanner keeps from one process's status file.
type ProcessStatus struct {
	// PeakRSSBytes is VmHWM, the high-water mark of resident memory since the
	// process started. The kernel maintains it continuously, so it catches the
	// spikes a sampled working-set gauge cannot (ADR 0006's stated gap).
	PeakRSSBytes int64
	// CPUsAllowed is how many CPUs Cpus_allowed_list permits. It is what
	// `sched_getaffinity` reports, which is not the CFS quota: a container
	// limited to half a core is still allowed on every CPU unless something
	// pinned it.
	CPUsAllowed int
}

// ReadProcessStatus reads the allow-listed fields of one process's status file.
// A missing or unreadable file yields the zero value: the process exited, or the
// kernel does not expose the field, and neither is worth a counter of its own —
// a fact that never arrives is one the payload's process count does not include.
func ReadProcessStatus(procRoot string, pid int) ProcessStatus {
	f, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "status")) // #nosec G304 -- procRoot is an operator-set flag; pid is a live process
	if err != nil {
		return ProcessStatus{}
	}
	defer func() { _ = f.Close() }()
	return parseProcessStatus(io.LimitReader(f, maxStatusBytes))
}

func parseProcessStatus(r io.Reader) ProcessStatus {
	var out ProcessStatus
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "VmHWM":
			out.PeakRSSBytes = peakBytes(value)
		case "Cpus_allowed_list":
			out.CPUsAllowed = countCPUList(value)
		}
	}
	return out
}

// peakBytes converts the kernel's "12345 kB" to bytes. A value in any other
// unit is not what this parser assumes and is dropped whole rather than
// misread by a factor of 1024.
func peakBytes(value string) int64 {
	number, unit, ok := strings.Cut(value, " ")
	if !ok || strings.TrimSpace(unit) != "kB" {
		return 0
	}
	kb, err := strconv.ParseInt(number, 10, 64)
	if err != nil || kb < 0 || kb > (1<<53)/1024 {
		return 0
	}
	return kb * 1024
}

// countCPUList counts the CPUs in a mask written as "0-3,8,12-15". Only the
// count leaves the node: which cores a container sits on says where its
// neighbours are, and no finding needs it.
func countCPUList(value string) int {
	total := 0
	for span := range strings.SplitSeq(value, ",") {
		if span == "" {
			continue
		}
		lo, hi, ranged := strings.Cut(span, "-")
		first, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil || first < 0 {
			return 0
		}
		if !ranged {
			total++
			continue
		}
		last, err := strconv.Atoi(strings.TrimSpace(hi))
		if err != nil || last < first {
			return 0
		}
		total += last - first + 1
	}
	return total
}
