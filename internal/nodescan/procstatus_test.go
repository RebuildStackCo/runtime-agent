package nodescan

import (
	"strings"
	"testing"
)

// A real status file, trimmed. `Name` is the point of the allow-list: it is the
// executable's own basename, and it sits two lines above the field we want.
const realWorldStatus = `Name:	checkout-api
Umask:	0022
State:	S (sleeping)
Tgid:	1
Pid:	1
PPid:	0
Uid:	65532	65532	65532	65532
VmPeak:	  2718392 kB
VmSize:	  2652856 kB
VmHWM:	   481280 kB
VmRSS:	   402136 kB
Threads:	17
Cpus_allowed:	ff
Cpus_allowed_list:	0-7
Mems_allowed_list:	0
`

func TestProcessStatusKeepsOnlyTheAllowedFields(t *testing.T) {
	got := parseProcessStatus(strings.NewReader(realWorldStatus))
	if want := int64(481280) * 1024; got.PeakRSSBytes != want {
		t.Errorf("peak = %d, want %d", got.PeakRSSBytes, want)
	}
	if got.CPUsAllowed != 8 {
		t.Errorf("cpus allowed = %d, want 8", got.CPUsAllowed)
	}
}

// The process name is an identity, and it is the field this parser is most
// likely to pick up by accident: it is line one of every status file. Nothing
// in ProcessStatus can hold it, and this test is what keeps that true when the
// struct grows.
func TestTheProcessNameHasNowhereToGo(t *testing.T) {
	got := parseProcessStatus(strings.NewReader(realWorldStatus))
	if want := (ProcessStatus{PeakRSSBytes: 481280 * 1024, CPUsAllowed: 8}); got != want {
		t.Errorf("status = %+v, want exactly the two allow-listed fields %+v", got, want)
	}
}

func TestPeakIsDroppedRatherThanMisread(t *testing.T) {
	cases := []struct {
		name string
		line string
		want int64
	}{
		{"kilobytes, as every kernel writes it", "VmHWM:\t   481280 kB", 481280 * 1024},
		{"no unit", "VmHWM:\t481280", 0},
		{"a unit this parser does not assume", "VmHWM:\t481280 MB", 0},
		{"not a number", "VmHWM:\tplenty kB", 0},
		{"absent", "VmRSS:\t402136 kB", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseProcessStatus(strings.NewReader(tc.line + "\n")).PeakRSSBytes; got != tc.want {
				t.Errorf("peak = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCPUListIsCounted(t *testing.T) {
	cases := []struct {
		list string
		want int
	}{
		{"0-7", 8},
		{"0", 1},
		{"0-3,8,12-15", 9},
		{"2-2", 1},
		{"", 0},
		{"3-1", 0},
		{"nonsense", 0},
	}
	for _, tc := range cases {
		t.Run(tc.list, func(t *testing.T) {
			got := parseProcessStatus(strings.NewReader("Cpus_allowed_list:\t" + tc.list + "\n"))
			if got.CPUsAllowed != tc.want {
				t.Errorf("cpus allowed for %q = %d, want %d", tc.list, got.CPUsAllowed, tc.want)
			}
		})
	}
}

// A file the parser cannot open is not an error path with a counter: the
// process simply contributes nothing, and the payload's process count says so.
func TestAMissingStatusFileYieldsNothing(t *testing.T) {
	if got := ReadProcessStatus(t.TempDir(), 1); got != (ProcessStatus{}) {
		t.Errorf("status of a missing file = %+v, want the zero value", got)
	}
}
