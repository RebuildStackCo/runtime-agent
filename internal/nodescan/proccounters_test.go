package nodescan

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A real stat line. Field two is the executable's name in parentheses, and this
// one contains both a space and a closing paren — the case that breaks every
// parser that splits the line on spaces.
const realWorldStat = `1 (checkout api (v2)) S 0 1 1 0 -1 4194560 91823 0 1841 0 4210 1337 0 0 20 0 17 0 8641234 2652856320 100534 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0 0 0 0 0 0 0 0 0`

const realWorldSchedStat = "48210934812 13440928374 91823\n"

const realWorldIO = `rchar: 90128374
wchar: 1209384
syscr: 88123
syscw: 4410
read_bytes: 268435456
write_bytes: 134217728
cancelled_write_bytes: 0
`

func writeCounterFiles(t *testing.T, root string, pid int, stat, sched, procIO string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"stat": stat, "schedstat": sched, "io": procIO} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The name in field two is the identity this parser must not pick up, and
// counting fields from the last ')' is what keeps it behind the cut rather than
// filtered after it (ADR 0062 §1).
func TestStatFieldsAreCountedFromTheNameNotThroughIt(t *testing.T) {
	root := t.TempDir()
	writeCounterFiles(t, root, 1, realWorldStat, "", "")

	utime, stime, majflt, start, ok := readProcStat(root, 1)
	if !ok {
		t.Fatal("a stat line with a space and a paren in the name was not parsed")
	}
	if utime != 4210 || stime != 1337 {
		t.Errorf("utime/stime = %d/%d ticks, want 4210/1337", utime, stime)
	}
	if majflt != 1841 {
		t.Errorf("major faults = %d, want 1841", majflt)
	}
	if start != 8641234 {
		t.Errorf("start ticks = %d, want 8641234", start)
	}
}

func TestSchedStatAndIOTakeOnlyWhatTheyNeed(t *testing.T) {
	root := t.TempDir()
	writeCounterFiles(t, root, 1, realWorldStat, realWorldSchedStat, realWorldIO)

	onCPU, runDelay := readSchedStat(root, 1)
	if onCPU != 48210934812 || runDelay != 13440928374 {
		t.Errorf("on cpu/run delay = %d/%d", onCPU, runDelay)
	}
	read, written := readProcIO(root, 1)
	if read != 268435456 || written != 134217728 {
		t.Errorf("read/write = %d/%d, want the block-storage counters", read, written)
	}
	// rchar and wchar count bytes asked for rather than bytes that moved, and
	// they sit directly above the two that are read.
	if read == 90128374 || written == 1209384 {
		t.Error("rchar/wchar were read as the block-storage counters")
	}
}

// A kernel without CONFIG_SCHEDSTATS has no such file, and a container may deny
// the io file. Both are zero rather than a failed reading: the rest of the
// counters are still worth having.
func TestMissingCounterFilesAreZeroNotFailure(t *testing.T) {
	root := t.TempDir()
	writeCounterFiles(t, root, 1, realWorldStat, "", "")

	c, _, ok := readProcessCounters(root, 1, ProcessStatus{})
	if !ok {
		t.Fatal("a process with only stat produced no reading")
	}
	if c.OnCPUNanos != 0 || c.ReadBytes != 0 {
		t.Errorf("absent files produced %+v, want zeros", c)
	}
	if c.CPUUserNanos != 4210*nanosPerTick {
		t.Errorf("cpu user = %d, want the stat reading converted from ticks", c.CPUUserNanos)
	}
}

// statWith rewrites the counter fields of the fixture: utime, stime and the
// start time, which is what says whether two readings are of one process.
func statWith(utime, stime, startTicks int64) string {
	return "1 (checkout api (v2)) S 0 1 1 0 -1 4194560 91823 0 1841 0 " +
		strconv.FormatInt(utime, 10) + " " + strconv.FormatInt(stime, 10) +
		" 0 0 20 0 17 0 " + strconv.FormatInt(startTicks, 10) + " 2652856320 100534"
}

// The first pass has nothing to subtract from, and says so by producing no
// delta at all rather than a delta equal to the whole life of the process.
func TestTheFirstReadingOfAProcessIsNotADelta(t *testing.T) {
	root := t.TempDir()
	writeCounterFiles(t, root, 1, statWith(100, 50, 8641234), realWorldSchedStat, realWorldIO)

	c := newCounterCache()
	now := time.Unix(1000, 0)
	if d := c.delta(root, 1, now, ProcessStatus{}); d != nil {
		t.Errorf("first reading produced a delta %+v", d)
	}
	writeCounterFiles(t, root, 1, statWith(160, 70, 8641234), realWorldSchedStat, realWorldIO)
	d := c.delta(root, 1, now.Add(60*time.Second), ProcessStatus{})
	if d == nil {
		t.Fatal("the second reading of the same process produced no delta")
	}
	if d.CPUUserNanos != 60*nanosPerTick || d.CPUSystemNanos != 20*nanosPerTick {
		t.Errorf("cpu delta = %d/%d, want the difference of the two readings",
			d.CPUUserNanos, d.CPUSystemNanos)
	}
	if d.ObservedNanos != int64(60*time.Second) {
		t.Errorf("observed = %d, want the wall-clock time between the readings", d.ObservedNanos)
	}
}

// A PID is reused. Two readings under one number then belong to two processes,
// and subtracting them yields a number about neither — the failure this cache's
// start-time key exists to prevent.
func TestAReusedPIDIsNotSubtractedFromItsPredecessor(t *testing.T) {
	root := t.TempDir()
	writeCounterFiles(t, root, 1, statWith(5000, 900, 8641234), realWorldSchedStat, realWorldIO)

	c := newCounterCache()
	now := time.Unix(1000, 0)
	c.delta(root, 1, now, ProcessStatus{})

	// The same PID, a lower counter, and a different start time: a new process.
	writeCounterFiles(t, root, 1, statWith(12, 3, 9000000), realWorldSchedStat, realWorldIO)
	if d := c.delta(root, 1, now.Add(60*time.Second), ProcessStatus{}); d != nil {
		t.Errorf("a reused PID produced a delta %+v against its predecessor", d)
	}
	// And the pass after that compares against the new process, not the old.
	writeCounterFiles(t, root, 1, statWith(20, 5, 9000000), realWorldSchedStat, realWorldIO)
	d := c.delta(root, 1, now.Add(120*time.Second), ProcessStatus{})
	if d == nil || d.CPUUserNanos != 8*nanosPerTick {
		t.Errorf("delta after re-baselining = %+v, want 8 ticks of user time", d)
	}
}

// A monotonic counter cannot fall. One that did means the reading is not of the
// process the last one described, whatever the start time claims, so the whole
// delta goes rather than one field being clamped to zero.
func TestACounterThatFellDropsTheWholeDelta(t *testing.T) {
	root := t.TempDir()
	writeCounterFiles(t, root, 1, statWith(5000, 900, 8641234), realWorldSchedStat, realWorldIO)

	c := newCounterCache()
	now := time.Unix(1000, 0)
	c.delta(root, 1, now, ProcessStatus{})

	writeCounterFiles(t, root, 1, statWith(4000, 900, 8641234), realWorldSchedStat, realWorldIO)
	if d := c.delta(root, 1, now.Add(60*time.Second), ProcessStatus{}); d != nil {
		t.Errorf("a counter that fell produced a delta %+v", d)
	}
}

// The cache holds one entry per live process rather than growing with every
// process the node has ever run.
func TestTheCacheForgetsProcessesThatAreGone(t *testing.T) {
	root := t.TempDir()
	writeCounterFiles(t, root, 1, statWith(100, 50, 8641234), realWorldSchedStat, realWorldIO)
	writeCounterFiles(t, root, 2, statWith(100, 50, 8641234), realWorldSchedStat, realWorldIO)

	c := newCounterCache()
	now := time.Unix(1000, 0)
	c.startPass()
	c.delta(root, 1, now, ProcessStatus{})
	c.delta(root, 2, now, ProcessStatus{})
	c.endPass()
	if len(c.last) != 2 {
		t.Fatalf("cache holds %d entries after a pass that saw two", len(c.last))
	}

	c.startPass()
	c.delta(root, 1, now.Add(time.Minute), ProcessStatus{})
	c.endPass()
	if len(c.last) != 1 {
		t.Errorf("cache holds %d entries after a pass that saw one", len(c.last))
	}
}

// The two switch counts come from the status file the scanner already read, so
// they arrive through the reading rather than through a fourth open.
func TestSwitchCountsRideTheStatusRead(t *testing.T) {
	root := t.TempDir()
	writeCounterFiles(t, root, 1, statWith(100, 50, 8641234), realWorldSchedStat, realWorldIO)

	c := newCounterCache()
	now := time.Unix(1000, 0)
	c.delta(root, 1, now, ProcessStatus{VoluntarySwitches: 1000, NonvoluntarySwitches: 10})
	d := c.delta(root, 1, now.Add(time.Minute), ProcessStatus{VoluntarySwitches: 1600, NonvoluntarySwitches: 42})
	if d == nil || d.VoluntarySwitches != 600 || d.NonvoluntarySwitches != 32 {
		t.Errorf("switch deltas = %+v, want 600 and 32", d)
	}
}
