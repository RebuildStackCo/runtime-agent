// Package nodescan is the node role's binary scanner: it enumerates processes,
// reads each executable's Go build information, ties it to a pod and container
// through the cgroup, and filters out infrastructure on the node. No Kubernetes
// client, no external egress — everything it needs is under /proc (ADR 0009).
//
// Only aggregate counts and the metadata of kept binaries leave here; identities
// of filtered-out ones leave as a number (CLAUDE.md invariant 6).
package nodescan

import (
	"debug/buildinfo"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BinaryInfo is what the scanner extracts about one kept Go process. It is the
// full record for a customer workload binary; infrastructure binaries never
// reach this shape (only the FilteredInfra counter moves for them).
type BinaryInfo struct {
	PID          int      `json:"pid"`
	GoVersion    string   `json:"go_version"`
	MainModule   string   `json:"main_module"`
	Dependencies []Module `json:"dependencies,omitempty"`
	// Settings is the allow-listed subset of the binary's build settings —
	// toolchain and target facts only, never the free-form flags the build
	// operator wrote (see buildSettingsAllowList). Absent keys mean the toolchain
	// did not record them, which for vcs.* is the common case in container
	// builds (ADR 0019).
	Settings map[string]string `json:"settings,omitempty"`
	// PGO is true when the binary was built with profile-guided optimization
	// (a "-pgo" build setting with a non-empty value).
	PGO bool `json:"pgo"`
	// PodUID and ContainerID come from the process cgroup; either may be empty
	// for a host process outside the kubepods hierarchy.
	PodUID      string `json:"pod_uid,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
}

// Module is one dependency of a build: the module path and the version the
// toolchain recorded for it (ADR 0048 §3).
type Module struct {
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	// Replaced marks a dependency a `replace` directive redirected, so a version
	// that does not match what is linked is legible as such rather than wrong.
	// The replacement's own path and version are not read: a local replacement's
	// path is a directory on the build machine, which is the class of string the
	// build-settings allow-list exists to keep out (ADR 0019).
	Replaced bool `json:"replaced,omitempty"`
}

// Counters are the only thing the scanner reports about what it did not keep.
// They are cumulative-free: each Scan returns the counts for that pass.
type Counters struct {
	// ProcessesScanned is every PID the pass attempted.
	ProcessesScanned int `json:"processes_scanned"`
	// GoFound is Go binaries kept after filtering — the customer workloads.
	GoFound int `json:"go_found"`
	// FilteredScope is processes dropped because their pod is not in the
	// controller-provided scope: a pod outside the customer's namespace filters,
	// a pod that opted out, or a host process belonging to no pod at all. Their
	// executables are never read — the drop happens on the cgroup, before any
	// build information exists (CLAUDE.md invariant 4).
	FilteredScope int `json:"filtered_scope"`
	// FilteredInfra is Go binaries dropped on the node by the module-path
	// deny-list (infrastructure and this agent itself).
	FilteredInfra int `json:"filtered_infra"`
	// Unreadable is a real executable from which no Go build information could
	// be recovered: a non-Go program, a Go binary whose build info was removed
	// (e.g. an aggressive external strip), or an exe we lacked permission to
	// read. Transient misses (a process that exited mid-scan, a kernel thread
	// with no executable) are not counted here.
	Unreadable int `json:"unreadable"`
}

// Result is one scan pass: the kept binaries and the aggregate counters.
type Result struct {
	Binaries []BinaryInfo
	Counters Counters
}

// Scanner reads Go build info from processes under a /proc-shaped tree and
// filters infrastructure by module path. It is safe to reuse across passes and
// carries no cumulative state.
type Scanner struct {
	procRoot string
	filter   *ModuleFilter
}

// NewScanner builds a scanner rooted at procRoot (normally "/proc"; a fixture
// tree in tests) using filter to classify module paths.
func NewScanner(procRoot string, filter *ModuleFilter) *Scanner {
	return &Scanner{procRoot: procRoot, filter: filter}
}

// Scan performs one full pass over the process tree, restricted to the pods
// scope admits, and returns the kept binaries and the counters. An individual
// unreadable process is counted, not surfaced; only a failure to read the
// process tree itself is an error.
//
// scope is required and its zero value admits nothing, so no call site can scan
// the node unscoped by omission.
func (s *Scanner) Scan(scope Scope) (Result, error) {
	entries, err := os.ReadDir(s.procRoot)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, entry := range entries {
		pid, ok := pidFromName(entry.Name())
		if !ok {
			continue
		}
		res.Counters.ProcessesScanned++
		s.scanPID(pid, scope, &res)
	}
	return res, nil
}

// scanPID reads one process's cgroup and, if its pod is in scope, its
// executable, updating res in place.
//
// The cgroup is read first on purpose. A process outside the scope has its
// executable left unopened and its module path never extracted: nothing about it
// is collected, rather than collected and dropped (CLAUDE.md invariant 4). Only
// the aggregate count moves.
func (s *Scanner) scanPID(pid int, scope Scope, res *Result) {
	binding := s.binding(pid)
	if !scope.Admits(binding.PodUID) {
		res.Counters.FilteredScope++
		return
	}

	exePath := filepath.Join(s.procRoot, strconv.Itoa(pid), "exe")
	info, err := buildinfo.ReadFile(exePath)
	if err != nil {
		if isTransientProcError(err) {
			// The process exited between listing and reading, or is a kernel
			// thread with no executable. Not an unreadable binary.
			return
		}
		// A real executable we could not extract Go build info from.
		res.Counters.Unreadable++
		return
	}

	mainModule := info.Path
	if info.Main.Path != "" {
		mainModule = info.Main.Path
	}
	if s.filter.IsInfra(mainModule) {
		res.Counters.FilteredInfra++
		return
	}

	res.Counters.GoFound++
	res.Binaries = append(res.Binaries, BinaryInfo{
		PID:          pid,
		GoVersion:    info.GoVersion,
		MainModule:   mainModule,
		Dependencies: dependencyModules(info),
		Settings:     buildSettings(info),
		PGO:          hasPGO(info),
		PodUID:       binding.PodUID,
		ContainerID:  binding.ContainerID,
	})
}

// binding reads and parses the process cgroup. A missing or unreadable cgroup
// file yields the zero binding — the process is still a valid find, just
// unattributed to a pod.
func (s *Scanner) binding(pid int) PodBinding {
	return ReadBinding(s.procRoot, pid)
}

// ReadBinding reads and parses the cgroup of process pid under procRoot,
// returning the pod UID and container ID the kubelet encodes into the cgroup
// path (ADR 0009 §2). A missing or unreadable cgroup yields the zero binding. It
// is exported so the node profiler can attribute a sample's PID to a pod and
// container without duplicating the parsing.
func ReadBinding(procRoot string, pid int) PodBinding {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup")) // #nosec G304 -- procRoot is an operator-set flag; pid is a live process
	if err != nil {
		return PodBinding{}
	}
	return parseCgroup(string(raw))
}

// dependencyModules returns the binary's dependencies, in the order the
// toolchain recorded them. A replaced module is reported under the path and
// version the build required, flagged, and never under its replacement's.
func dependencyModules(info *buildinfo.BuildInfo) []Module {
	if len(info.Deps) == 0 {
		return nil
	}
	mods := make([]Module, 0, len(info.Deps))
	for _, d := range info.Deps {
		if d == nil {
			continue
		}
		mods = append(mods, Module{Path: d.Path, Version: d.Version, Replaced: d.Replace != nil})
	}
	return mods
}

// buildSettingsAllowList is the exhaustive set of build settings the scanner
// keeps. An allow-list is the decision, not the mechanism (ADR 0019): settings
// are where strings written by whoever ran the build enter the agent, and
// "-ldflags", "-pgo", "-gcflags" and "-tags" are all free-form. A deny-list
// would cover only the keys we thought to name.
//
// Everything here is a bounded token the toolchain chose, or a vcs.* value git
// produced. GOOS is absent: every process the agent can see is linux.
var buildSettingsAllowList = map[string]struct{}{
	"CGO_ENABLED":  {},
	"GOARCH":       {},
	"GOAMD64":      {}, // x86-64 microarchitecture level: v1 on a v3 fleet leaves instructions unused
	"GOARM64":      {},
	"GOARM":        {},
	"-race":        {}, // a race build in production costs multiples of CPU and memory
	"-trimpath":    {},
	"vcs":          {},
	"vcs.revision": {},
	"vcs.time":     {},
	"vcs.modified": {},
}

// maxSettingValue bounds a kept value. Every allowed key holds something short
// and bounded — a commit hash, an RFC 3339 timestamp, "0"/"1", "v8.0" — so a
// longer value means the binary is not what this list assumes. Such a setting is
// dropped rather than truncated: a prefix of an unexpected string is still an
// unexpected string.
const maxSettingValue = 128

// buildSettings returns the allow-listed build settings of the binary. The
// result is nil when none were recorded, and marshals with sorted keys (Go sorts
// string map keys), so the payload bytes stay deterministic — the golden
// contract.
func buildSettings(info *buildinfo.BuildInfo) map[string]string {
	var out map[string]string
	for _, s := range info.Settings {
		if _, ok := buildSettingsAllowList[s.Key]; !ok {
			continue
		}
		if s.Value == "" || len(s.Value) > maxSettingValue {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(buildSettingsAllowList))
		}
		out[s.Key] = s.Value
	}
	return out
}

// hasPGO reports whether the binary was built with profile-guided optimization.
// The toolchain records this as a "-pgo" build setting whose value is the
// profile path ("off" or empty means no PGO). Only the boolean survives: the
// path names a directory on the build machine, so it is not allow-listed and
// never leaves the node.
func hasPGO(info *buildinfo.BuildInfo) bool {
	for _, s := range info.Settings {
		if s.Key == "-pgo" {
			return s.Value != "" && s.Value != "off"
		}
	}
	return false
}

// pidFromName returns the PID if name is an all-digits directory (a process
// entry), and false for the many non-PID entries in /proc (self, meminfo, …).
func pidFromName(name string) (int, bool) {
	pid, err := strconv.Atoi(name)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// isTransientProcError reports whether err is the ordinary churn of scanning a
// live process table — the process exited, or the /proc entry vanished — rather
// than a binary we genuinely could not read. buildinfo wraps the underlying os
// error, so we unwrap to the fs error and to ENOENT/ESRCH.
func isTransientProcError(err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	// A kernel thread's /proc/<pid>/exe is an empty target; opening it fails
	// with ENOENT, already covered above. Some kernels surface ESRCH as the
	// process disappears; match it by string since it has no portable sentinel.
	return strings.Contains(err.Error(), "no such process")
}
