// Package nodescan is the node role's binary scanner. It enumerates processes
// on the node, reads the Go build information embedded in each executable, ties
// it to a pod and container through the process's cgroup, and filters out
// infrastructure on the node. It holds no Kubernetes client and no external
// egress: everything it needs is under /proc (ADR 0009).
//
// Only aggregate counts and the metadata of kept (non-infrastructure) binaries
// are produced here. Identities of filtered-out binaries never leave this
// package as anything but a number (CLAUDE.md invariant 6).
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
	Dependencies []string `json:"dependencies,omitempty"`
	// PGO is true when the binary was built with profile-guided optimization
	// (a "-pgo" build setting with a non-empty value).
	PGO bool `json:"pgo"`
	// PodUID and ContainerID come from the process cgroup; either may be empty
	// for a host process outside the kubepods hierarchy.
	PodUID      string `json:"pod_uid,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
}

// Counters are the only thing the scanner reports about what it did not keep.
// They are cumulative-free: each Scan returns the counts for that pass.
type Counters struct {
	// ProcessesScanned is every PID the pass attempted.
	ProcessesScanned int `json:"processes_scanned"`
	// GoFound is Go binaries kept after filtering — the customer workloads.
	GoFound int `json:"go_found"`
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

// Scan performs one full pass over the process tree and returns the kept
// binaries and the counters. It never returns an error for an individual
// unreadable process — those are counted, not surfaced — only for a failure to
// read the process tree itself.
func (s *Scanner) Scan() (Result, error) {
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
		s.scanPID(pid, &res)
	}
	return res, nil
}

// scanPID reads one process's executable and cgroup, updating res in place.
func (s *Scanner) scanPID(pid int, res *Result) {
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

	binding := s.binding(pid)
	res.Counters.GoFound++
	res.Binaries = append(res.Binaries, BinaryInfo{
		PID:          pid,
		GoVersion:    info.GoVersion,
		MainModule:   mainModule,
		Dependencies: dependencyPaths(info),
		PGO:          hasPGO(info),
		PodUID:       binding.PodUID,
		ContainerID:  binding.ContainerID,
	})
}

// binding reads and parses the process cgroup. A missing or unreadable cgroup
// file yields the zero binding — the process is still a valid find, just
// unattributed to a pod.
func (s *Scanner) binding(pid int) PodBinding {
	raw, err := os.ReadFile(filepath.Join(s.procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return PodBinding{}
	}
	return parseCgroup(string(raw))
}

// dependencyPaths returns the module paths of the binary's dependencies, in the
// order the toolchain recorded them.
func dependencyPaths(info *buildinfo.BuildInfo) []string {
	if len(info.Deps) == 0 {
		return nil
	}
	paths := make([]string, 0, len(info.Deps))
	for _, d := range info.Deps {
		if d != nil {
			paths = append(paths, d.Path)
		}
	}
	return paths
}

// hasPGO reports whether the binary was built with profile-guided optimization.
// The toolchain records this as a "-pgo" build setting whose value is the
// profile path ("off" or empty means no PGO).
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
