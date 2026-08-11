package ebpfgate

import (
	"os"
	"path/filepath"
	"testing"
)

// makeRoots builds a fake procRoot/sysRoot pair. writeOS=false simulates an
// unreadable osrelease (e.g. the mount is missing); btf toggles the presence of
// <sysRoot>/kernel/btf/vmlinux.
func makeRoots(t *testing.T, osrelease string, writeOS, btf bool) (proc, sys string) {
	t.Helper()
	proc, sys = t.TempDir(), t.TempDir()
	if writeOS {
		dir := filepath.Join(proc, "sys", "kernel")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "osrelease"), []byte(osrelease), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if btf {
		dir := filepath.Join(sys, "kernel", "btf")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "vmlinux"), []byte("btf"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return proc, sys
}

func TestProbe(t *testing.T) {
	tests := []struct {
		name       string
		osrelease  string
		writeOS    bool
		btf        bool
		wantReason Reason
		wantBTF    bool
		wantMajor  int
		wantMinor  int
	}{
		{"modern gcp kernel", "6.8.0-1064-gcp\n", true, true, ReasonSupported, true, 6, 8},
		{"exactly 5.8", "5.8.0", true, true, ReasonSupported, true, 5, 8},
		{"5.15 linuxkit with btf", "5.15.49-linuxkit", true, true, ReasonSupported, true, 5, 15},
		{"minor without patch", "6.8", true, true, ReasonSupported, true, 6, 8},

		{"supported version but no btf", "6.8.0", true, false, ReasonBTFAbsent, false, 6, 8},
		{"5.8 without btf", "5.8.0", true, false, ReasonBTFAbsent, false, 5, 8},

		{"just below floor", "5.7.19", true, true, ReasonKernelTooOld, true, 5, 7},
		{"old 5.4", "5.4.0", true, true, ReasonKernelTooOld, true, 5, 4},
		// RHEL backports BTF onto 4.18: BTF present but version < 5.8 -> refused on
		// version, and BTFPresent must still be reported true (constraint b).
		{"rhel 4.18 with backported btf", "4.18.0-425.el8.x86_64", true, true, ReasonKernelTooOld, true, 4, 18},
		// version dominates btf: too old AND no btf -> kernel_too_old.
		{"too old and no btf", "5.4.0", true, false, ReasonKernelTooOld, false, 5, 4},

		{"garbage osrelease", "not-a-version", true, true, ReasonKernelUnknown, true, 0, 0},
		{"empty osrelease", "", true, true, ReasonKernelUnknown, true, 0, 0},
		{"missing osrelease file", "", false, true, ReasonKernelUnknown, true, 0, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proc, sys := makeRoots(t, tc.osrelease, tc.writeOS, tc.btf)
			got := Probe(proc, sys)
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.BTFPresent != tc.wantBTF {
				t.Errorf("BTFPresent = %v, want %v", got.BTFPresent, tc.wantBTF)
			}
			if got.Major != tc.wantMajor || got.Minor != tc.wantMinor {
				t.Errorf("version = %d.%d, want %d.%d", got.Major, got.Minor, tc.wantMajor, tc.wantMinor)
			}
			if got.Supported() != (tc.wantReason == ReasonSupported) {
				t.Errorf("Supported() = %v, want %v", got.Supported(), tc.wantReason == ReasonSupported)
			}
		})
	}
}

func TestParseKernelVersion(t *testing.T) {
	tests := []struct {
		in           string
		major, minor int
		ok           bool
	}{
		{"6.8.0-1064-gcp", 6, 8, true},
		{"5.15.49-linuxkit", 5, 15, true},
		{"5.8", 5, 8, true},
		{"  5.4.0\n", 5, 4, true},
		{"6", 0, 0, false},
		{"", 0, 0, false},
		{"garbage", 0, 0, false},
	}
	for _, tc := range tests {
		major, minor, ok := parseKernelVersion(tc.in)
		if ok != tc.ok || (ok && (major != tc.major || minor != tc.minor)) {
			t.Errorf("parseKernelVersion(%q) = %d,%d,%v; want %d,%d,%v",
				tc.in, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
	}
}
