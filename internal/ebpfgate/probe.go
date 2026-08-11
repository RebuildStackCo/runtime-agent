// Package ebpfgate decides whether a node can run the eBPF CPU profiler
// (ADR 0011). It is a readiness *gate*, evaluated once before the profiler is
// ever started: on a kernel that is too old or lacks BTF the node refuses the
// `ebpf` profile gracefully and keeps running the Go-binary scanner, rather
// than escalating to `privileged` or crashing.
//
// The kernel version is a proxy, not the truth: it is cheap to read and rules
// out kernels where CAP_BPF/CAP_PERFMON do not even exist (< 5.8), but a Probe
// that passes does not guarantee the profiler will attach — LSM, kernel
// lockdown, or perf_event_paranoid can still refuse at load time. That final
// gate lives with the profiler (ADR 0011; the profiler slice reports a
// program-load failure with the same graceful discipline). The reasons here are
// deliberately split (kernel too old vs BTF absent vs unknown) so the counts
// tell us *why* a fleet refused — in particular so a BTF-capable-but-old kernel
// (RHEL backports BTF onto 4.18/5.14) is visibly refused on version alone, which
// is what we would relax first if we ever support it.
package ebpfgate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Reason is why a node is (not) eBPF-ready. It is stable, low-cardinality, and
// meant to be counted.
type Reason string

// Reasons a node is or is not eBPF-ready. The refusal reasons are kept distinct
// so their counts show why a fleet declined (constraint b).
const (
	ReasonSupported     Reason = "supported"
	ReasonKernelTooOld  Reason = "kernel_too_old" // kernel older than 5.8
	ReasonBTFAbsent     Reason = "btf_absent"     // /sys/kernel/btf/vmlinux missing
	ReasonKernelUnknown Reason = "kernel_unknown" // osrelease unreadable/unparseable
	// ReasonProgramLoadFailed is set by the capture driver, not Probe: the
	// version gate can pass yet the eBPF programs still fail to load or attach
	// (LSM, kernel lockdown, perf_event_paranoid). It is handled with the same
	// graceful discipline as a too-old kernel (ADR 0011 §2, constraint b).
	ReasonProgramLoadFailed Reason = "program_load_failed"
)

// Minimum kernel for CAP_BPF/CAP_PERFMON (ADR 0011, security.md §7.2).
const (
	minKernelMajor = 5
	minKernelMinor = 8
)

// Result is the gate outcome. BTFPresent and KernelVersion are populated for
// reporting even when the Reason is a refusal, so a refused node still tells us
// what it saw.
type Result struct {
	Reason        Reason
	KernelVersion string // raw osrelease, e.g. "6.8.0-1064-gcp"
	Major, Minor  int    // parsed version; zero when unknown
	BTFPresent    bool
}

// Supported reports whether the profiler may be started.
func (r Result) Supported() bool { return r.Reason == ReasonSupported }

// Probe evaluates readiness from two roots so it is pure and testable: the
// kernel version from <procRoot>/sys/kernel/osrelease and BTF from
// <sysRoot>/kernel/btf/vmlinux. In the DaemonSet procRoot is the host /proc
// mount and sysRoot is the read-only /sys/kernel/btf host mount's parent.
//
// Precedence when several things are wrong: an unknown version, then a
// too-old version, then absent BTF. Version dominates BTF so that the RHEL
// backport case (BTF present, version < 5.8) is reported as kernel_too_old, not
// masked as supported.
func Probe(procRoot, sysRoot string) Result {
	var res Result

	// BTF as a fact, recorded regardless of the version verdict.
	if _, err := os.Stat(filepath.Join(sysRoot, "kernel", "btf", "vmlinux")); err == nil {
		res.BTFPresent = true
	}

	// #nosec G304 -- procRoot is an operator-provided mount path (a flag), not user input
	raw, err := os.ReadFile(filepath.Join(procRoot, "sys", "kernel", "osrelease"))
	if err != nil {
		res.Reason = ReasonKernelUnknown
		return res
	}
	res.KernelVersion = strings.TrimSpace(string(raw))

	major, minor, ok := parseKernelVersion(res.KernelVersion)
	if !ok {
		res.Reason = ReasonKernelUnknown
		return res
	}
	res.Major, res.Minor = major, minor

	if major < minKernelMajor || (major == minKernelMajor && minor < minKernelMinor) {
		res.Reason = ReasonKernelTooOld
		return res
	}
	if !res.BTFPresent {
		res.Reason = ReasonBTFAbsent
		return res
	}
	res.Reason = ReasonSupported
	return res
}

// parseKernelVersion extracts the leading major.minor from an osrelease string
// such as "6.8.0-1064-gcp" or "5.15.49-linuxkit". It ignores everything after
// the minor number, so patch levels and vendor suffixes do not matter.
func parseKernelVersion(osrelease string) (major, minor int, ok bool) {
	s := strings.TrimSpace(osrelease)
	if n, err := fmt.Sscanf(s, "%d.%d", &major, &minor); err != nil || n != 2 {
		return 0, 0, false
	}
	return major, minor, true
}
