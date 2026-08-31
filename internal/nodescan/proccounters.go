package nodescan

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The counter reader (ADR 0062). Everything here is monotonic since the process
// started, so a reading alone says nothing about now: what a finding needs is
// the change over an interval, and this file is where that subtraction happens.
//
// It happens on the node because the node is the only place that can do it
// safely. A PID is reused, so two readings under one number can belong to two
// processes; the start time distinguishes them, and it is read alongside the
// counters rather than fetched afterwards (ADR 0043's discipline).

// userHZ is the unit of the CPU-time fields in /proc/<pid>/stat. It is 100 on
// every Linux architecture regardless of the kernel's own tick rate: USER_HZ is
// part of the kernel's ABI, not of its configuration.
const userHZ = 100

// nanosPerTick converts a clock tick to nanoseconds.
const nanosPerTick = int64(time.Second) / userHZ

// maxCounterFileBytes bounds each read. The three files are a few hundred bytes.
const maxCounterFileBytes = 16 << 10

// ProcessCounters is one reading of the monotonic counters of one process. Every
// field is cumulative since that process started, and none is meaningful on its
// own — CounterDelta below is what leaves the node.
type ProcessCounters struct {
	// OnCPUNanos is time spent running and RunDelayNanos time spent runnable
	// but waiting for a CPU. Their ratio is CPU starvation measured directly,
	// independent of whether a CFS quota was ever hit.
	OnCPUNanos    int64
	RunDelayNanos int64
	// CPUUserNanos and CPUSystemNanos split that running time between the
	// program and the kernel acting for it.
	CPUUserNanos   int64
	CPUSystemNanos int64
	// MajorFaults are page faults that had to reach storage: the working set
	// does not fit, rather than merely growing.
	MajorFaults int64
	// VoluntarySwitches is the process yielding — normal for a server waiting
	// on I/O. NonvoluntarySwitches is the scheduler taking the CPU away, which
	// is the same starvation RunDelayNanos measures, seen from the other side.
	VoluntarySwitches    int64
	NonvoluntarySwitches int64
	// ReadBytes and WriteBytes are bytes that actually reached block storage,
	// not bytes the process asked for: the page cache absorbs the difference.
	ReadBytes  int64
	WriteBytes int64
}

// CounterDelta is what changed between two readings of one process, and how much
// wall-clock time separates them. Both travel together: a delta without its
// interval is a number no consumer can turn into a rate (ADR 0062 §2).
type CounterDelta struct {
	ObservedNanos        int64 `json:"observed_nanos"`
	OnCPUNanos           int64 `json:"on_cpu_nanos,omitempty"`
	RunDelayNanos        int64 `json:"run_delay_nanos,omitempty"`
	CPUUserNanos         int64 `json:"cpu_user_nanos,omitempty"`
	CPUSystemNanos       int64 `json:"cpu_system_nanos,omitempty"`
	MajorFaults          int64 `json:"major_faults,omitempty"`
	VoluntarySwitches    int64 `json:"voluntary_switches,omitempty"`
	NonvoluntarySwitches int64 `json:"nonvoluntary_switches,omitempty"`
	ReadBytes            int64 `json:"read_bytes,omitempty"`
	WriteBytes           int64 `json:"write_bytes,omitempty"`
}

// reading is what the cache holds between passes: one process's counters, when
// they were read, and the start time that says which process they belong to.
type reading struct {
	at         time.Time
	startTicks int64
	counters   ProcessCounters
}

// counterCache holds the previous pass's reading per PID. It is the only state
// the node keeps between passes besides the marker cache, and it is
// loss-harmless: a restart costs one interval of counters, never a total, and
// the next pass re-baselines (ADR 0003).
type counterCache struct {
	last map[int]reading
	seen map[int]struct{}
}

func newCounterCache() *counterCache {
	return &counterCache{last: map[int]reading{}, seen: map[int]struct{}{}}
}

// delta reads one process's counters and returns the change since this cache's
// previous reading of the same process, or nil when there is nothing to compare
// against: the first pass that saw it, a PID that now belongs to a different
// process, or a counter that moved backwards.
func (c *counterCache) delta(procRoot string, pid int, now time.Time, status ProcessStatus) *CounterDelta {
	cur, startTicks, ok := readProcessCounters(procRoot, pid, status)
	if !ok {
		return nil
	}
	c.seen[pid] = struct{}{}
	prev, had := c.last[pid]
	c.last[pid] = reading{at: now, startTicks: startTicks, counters: cur}
	if !had || prev.startTicks != startTicks {
		// A process seen for the first time, or a PID that has been reused.
		// Neither has a previous reading of *this* process to subtract from.
		return nil
	}
	elapsed := now.Sub(prev.at)
	if elapsed <= 0 {
		return nil
	}
	d := CounterDelta{
		ObservedNanos:        elapsed.Nanoseconds(),
		OnCPUNanos:           cur.OnCPUNanos - prev.counters.OnCPUNanos,
		RunDelayNanos:        cur.RunDelayNanos - prev.counters.RunDelayNanos,
		CPUUserNanos:         cur.CPUUserNanos - prev.counters.CPUUserNanos,
		CPUSystemNanos:       cur.CPUSystemNanos - prev.counters.CPUSystemNanos,
		MajorFaults:          cur.MajorFaults - prev.counters.MajorFaults,
		VoluntarySwitches:    cur.VoluntarySwitches - prev.counters.VoluntarySwitches,
		NonvoluntarySwitches: cur.NonvoluntarySwitches - prev.counters.NonvoluntarySwitches,
		ReadBytes:            cur.ReadBytes - prev.counters.ReadBytes,
		WriteBytes:           cur.WriteBytes - prev.counters.WriteBytes,
	}
	// A monotonic counter cannot fall. One that did means the reading is not of
	// the process the previous one described, whatever the start time says, so
	// the whole delta is dropped rather than one field being clamped.
	if negative(d) {
		return nil
	}
	return &d
}

func negative(d CounterDelta) bool {
	return d.OnCPUNanos < 0 || d.RunDelayNanos < 0 || d.CPUUserNanos < 0 ||
		d.CPUSystemNanos < 0 || d.MajorFaults < 0 || d.VoluntarySwitches < 0 ||
		d.NonvoluntarySwitches < 0 || d.ReadBytes < 0 || d.WriteBytes < 0
}

// startPass begins a pass; endPass drops every PID the pass did not see, so the
// cache holds one entry per live process rather than growing with every process
// the node has ever run.
func (c *counterCache) startPass() { clear(c.seen) }

func (c *counterCache) endPass() {
	for pid := range c.last {
		if _, ok := c.seen[pid]; !ok {
			delete(c.last, pid)
		}
	}
}

// readProcessCounters assembles one reading from three files plus the two
// switch counts the status read already produced. It reports the process's
// start time separately: that value keys the reading rather than travelling
// with it, and it never leaves the node.
func readProcessCounters(procRoot string, pid int, status ProcessStatus) (ProcessCounters, int64, bool) {
	utime, stime, majflt, startTicks, ok := readProcStat(procRoot, pid)
	if !ok {
		return ProcessCounters{}, 0, false
	}
	onCPU, runDelay := readSchedStat(procRoot, pid)
	read, written := readProcIO(procRoot, pid)
	return ProcessCounters{
		OnCPUNanos:           onCPU,
		RunDelayNanos:        runDelay,
		CPUUserNanos:         utime * nanosPerTick,
		CPUSystemNanos:       stime * nanosPerTick,
		MajorFaults:          majflt,
		VoluntarySwitches:    status.VoluntarySwitches,
		NonvoluntarySwitches: status.NonvoluntarySwitches,
		ReadBytes:            read,
		WriteBytes:           written,
	}, startTicks, true
}

// readProcStat takes four fields of /proc/<pid>/stat by position.
//
// Field two is the executable's own name in parentheses, and it may itself
// contain spaces and parentheses — so the fields are counted from the last ')'
// rather than by splitting the line. That is also why the name never reaches a
// variable here: it is behind the cut, not filtered after it.
func readProcStat(procRoot string, pid int) (utime, stime, majflt, startTicks int64, ok bool) {
	body, err := readSmallFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, 0, 0, false
	}
	end := strings.LastIndexByte(body, ')')
	if end < 0 {
		return 0, 0, 0, 0, false
	}
	// After the name, field 3 is the first: index i holds field i+3.
	fields := strings.Fields(body[end+1:])
	at := func(field int) (int64, bool) {
		i := field - 3
		if i < 0 || i >= len(fields) {
			return 0, false
		}
		n, err := strconv.ParseInt(fields[i], 10, 64)
		return n, err == nil && n >= 0
	}
	majflt, okMaj := at(12)
	utime, okU := at(14)
	stime, okS := at(15)
	startTicks, okStart := at(22)
	if !okMaj || !okU || !okS || !okStart {
		return 0, 0, 0, 0, false
	}
	return utime, stime, majflt, startTicks, true
}

// readSchedStat takes the first two of the three numbers in
// /proc/<pid>/schedstat: nanoseconds on CPU, and nanoseconds runnable but
// waiting for one. A kernel built without CONFIG_SCHEDSTATS has no such file,
// and both are then zero.
func readSchedStat(procRoot string, pid int) (onCPU, runDelay int64) {
	body, err := readSmallFile(filepath.Join(procRoot, strconv.Itoa(pid), "schedstat"))
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(body)
	if len(fields) < 2 {
		return 0, 0
	}
	a, errA := strconv.ParseInt(fields[0], 10, 64)
	b, errB := strconv.ParseInt(fields[1], 10, 64)
	if errA != nil || errB != nil || a < 0 || b < 0 {
		return 0, 0
	}
	return a, b
}

// readProcIO takes the two block-storage counters. The file also holds rchar
// and wchar, which count bytes the process asked for rather than bytes that
// moved, and neither is read: a finding about storage rests on what reached it.
func readProcIO(procRoot string, pid int) (read, written int64) {
	f, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "io")) // #nosec G304 -- procRoot is an operator-set flag; pid is a live process
	if err != nil {
		return 0, 0
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(io.LimitReader(f, maxCounterFileBytes))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || n < 0 {
			continue
		}
		switch key {
		case "read_bytes":
			read = n
		case "write_bytes":
			written = n
		}
	}
	return read, written
}

func readSmallFile(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- path is built from an operator-set proc root and a live pid
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, maxCounterFileBytes))
	return string(body), err
}
