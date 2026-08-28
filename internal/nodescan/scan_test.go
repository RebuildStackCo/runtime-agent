package nodescan

import (
	"debug/buildinfo"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// buildGoBinary compiles a trivial main package in a throwaway module with the
// given module path and returns the binary path. The module path is what the
// scanner's filter keys on, so tests can mint "kept" and "infra" binaries at
// will. The build is fully offline: no imports beyond the standard library.
func buildGoBinary(t *testing.T, modulePath string) string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module "+modulePath+"\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatalf("writing main.go: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "prog")
	cmd := exec.Command("go", "build", "-o", bin, ".") //nolint:gosec // fixture build from a constant argv; all inputs are test-controlled
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", modulePath, err, out)
	}
	return bin
}

// procEntry describes one PID to stage under a fake /proc tree.
type procEntry struct {
	pid     int
	exe     string // path the exe symlink points at; "" means no exe (kernel thread / gone)
	cgroup  string // cgroup file contents; "" means no cgroup file
	rawFile bool   // if true, exe is a regular file with the literal bytes in exe, not a symlink
}

func stageProc(t *testing.T, entries []procEntry, extraFiles []string) string {
	t.Helper()
	root := t.TempDir()
	for _, e := range entries {
		dir := filepath.Join(root, strconv.Itoa(e.pid))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if e.exe != "" {
			target := filepath.Join(dir, "exe")
			if e.rawFile {
				if err := os.WriteFile(target, []byte(e.exe), 0o600); err != nil {
					t.Fatalf("writing exe file: %v", err)
				}
			} else if err := os.Symlink(e.exe, target); err != nil {
				t.Fatalf("symlink exe: %v", err)
			}
		}
		if e.cgroup != "" {
			if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(e.cgroup), 0o600); err != nil {
				t.Fatalf("writing cgroup: %v", err)
			}
		}
	}
	// Non-PID entries that a real /proc carries and the scanner must ignore.
	for _, name := range extraFiles {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("writing extra %s: %v", name, err)
		}
	}
	return root
}

func TestScannerClassifiesProcesses(t *testing.T) {
	keptBin := buildGoBinary(t, "example.com/team/payments")
	infraBin := buildGoBinary(t, "k8s.io/faux-component")

	const podUID = "1234abcd-12ab-34cd-56ef-1234567890ab"
	const containerID = "deadbeef00000000000000000000000000000000000000000000000000000000"
	keptCgroup := "0::/kubepods/burstable/pod" + podUID + "/" + containerID

	uid := func(n string) string { return "0000000" + n + "-12ab-34cd-56ef-1234567890ab" }
	cgroupFor := func(n string) string { return "0::/kubepods/besteffort/pod" + uid(n) + "/x" }

	root := stageProc(t, []procEntry{
		{pid: 100, exe: keptBin, cgroup: keptCgroup},
		{pid: 200, exe: infraBin, cgroup: cgroupFor("2")},
		{pid: 300, exe: "this is not an executable at all", rawFile: true, // unreadable
			cgroup: cgroupFor("3")},
		{pid: 400, cgroup: cgroupFor("4")}, // no exe: kernel thread / gone -> skipped
		// A pod outside the customer's filters, and a host process belonging to
		// no pod at all: both are dropped on the cgroup, before their
		// executables are read.
		{pid: 500, exe: keptBin, cgroup: cgroupFor("5")},
		{pid: 600, exe: keptBin},
	}, []string{"meminfo", "self"})

	s := NewScanner(root, NewModuleFilter(DefaultInfraModulePrefixes))
	res, err := s.Scan(NewScope([]string{podUID, uid("2"), uid("3"), uid("4")}))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	want := Counters{ProcessesScanned: 6, GoFound: 1, FilteredScope: 2, FilteredInfra: 1, Unreadable: 1}
	if res.Counters != want {
		t.Errorf("counters = %+v, want %+v", res.Counters, want)
	}

	if len(res.Binaries) != 1 {
		t.Fatalf("kept binaries = %d, want 1: %+v", len(res.Binaries), res.Binaries)
	}
	b := res.Binaries[0]
	if b.PID != 100 {
		t.Errorf("pid = %d, want 100", b.PID)
	}
	if b.MainModule != "example.com/team/payments" {
		t.Errorf("main module = %q, want example.com/team/payments", b.MainModule)
	}
	if b.GoVersion == "" {
		t.Error("go version is empty, want the toolchain version")
	}
	if b.PodUID != podUID {
		t.Errorf("pod uid = %q, want %q", b.PodUID, podUID)
	}
	if b.ContainerID != containerID {
		t.Errorf("container id = %q, want %q", b.ContainerID, containerID)
	}
}

// A node that has not obtained a scope must not scan. docs/security.md §10.2
// promises customers that node-level samples outside their filters are dropped
// on the node before transport, and the node cannot make that claim on its own —
// its cgroup gives it a pod UID, never a namespace.
func TestScannerWithoutScopeScansNothing(t *testing.T) {
	keptBin := buildGoBinary(t, "example.com/team/payments")
	root := stageProc(t, []procEntry{
		{pid: 100, exe: keptBin, cgroup: "0::/kubepods/burstable/pod00000001-12ab-34cd-56ef-1234567890ab/x"},
		{pid: 300, exe: "this is not an executable at all", rawFile: true,
			cgroup: "0::/kubepods/besteffort/pod00000003-12ab-34cd-56ef-1234567890ab/x"},
	}, nil)

	s := NewScanner(root, NewModuleFilter(DefaultInfraModulePrefixes))
	res, err := s.Scan(DenyAll())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(res.Binaries) != 0 {
		t.Fatalf("kept binaries = %d, want 0 without a scope: %+v", len(res.Binaries), res.Binaries)
	}
	if res.Counters.FilteredScope != 2 || res.Counters.ProcessesScanned != 2 {
		t.Errorf("counters = %+v, want every process counted as out of scope", res.Counters)
	}
	// Unreadable stayed zero even though a non-executable file was staged: an
	// out-of-scope process has its executable left unopened, so nothing about it
	// is collected rather than collected and dropped (CLAUDE.md invariant 4).
	if res.Counters.Unreadable != 0 {
		t.Errorf("unreadable = %d, want 0 — an out-of-scope executable must never be read",
			res.Counters.Unreadable)
	}
}

func TestScopeAdmits(t *testing.T) {
	scope := NewScope([]string{"pod-a", "pod-b", ""})
	if scope.Size() != 2 {
		t.Errorf("size = %d, want 2 (the empty UID is not a pod)", scope.Size())
	}
	if !scope.Admits("pod-a") {
		t.Error("an admitted pod must be admitted")
	}
	if scope.Admits("pod-c") {
		t.Error("a pod outside the controller's answer must not be admitted")
	}
	// A host process — kubelet, a systemd unit — belongs to no pod, so no
	// namespace filter can permit it.
	if scope.Admits("") {
		t.Error("a process with no pod must never be admitted")
	}
	if DenyAll().Admits("pod-a") {
		t.Error("the denying scope must admit nothing")
	}
}

// A replaced module is reported under what the build required, never under the
// replacement: a local `replace` puts a build-machine directory in Replace.Path
// (ADR 0048 §3).
func TestDependencyModules(t *testing.T) {
	info := &buildinfo.BuildInfo{
		Deps: []*debug.Module{
			{Path: "example.com/a", Version: "v1.0.0"},
			nil, // the toolchain can leave holes; must be skipped, not panic
			{Path: "example.com/b", Version: "v2.3.4"},
			{Path: "example.com/c", Version: "v0.1.0", Replace: &debug.Module{
				Path: "/home/builder/src/c", Version: "",
			}},
		},
	}
	got := dependencyModules(info)
	want := []Module{
		{Path: "example.com/a", Version: "v1.0.0"},
		{Path: "example.com/b", Version: "v2.3.4"},
		{Path: "example.com/c", Version: "v0.1.0", Replaced: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("dependencyModules = %v, want %v", got, want)
	}
	if dependencyModules(&buildinfo.BuildInfo{}) != nil {
		t.Error("dependencyModules of a depless binary should be nil")
	}
}

// The real settings block of a binary built in a container, as `go version -m`
// prints it. Everything the toolchain records is here, including the three keys
// that must never leave the node.
func realWorldSettings() []debug.BuildSetting {
	return []debug.BuildSetting{
		{Key: "-buildmode", Value: "exe"},
		{Key: "-compiler", Value: "gc"},
		{Key: "-ldflags", Value: `-X main.version=a5edd4b-dirty -X main.buildHost=ci-07.internal.acme.corp`},
		{Key: "-pgo", Value: "/home/jenkins/workspace/acme-web/default.pgo"},
		{Key: "-trimpath", Value: "true"},
		{Key: "CGO_ENABLED", Value: "0"},
		{Key: "GOARCH", Value: "arm64"},
		{Key: "GOOS", Value: "linux"},
		{Key: "GOARM64", Value: "v8.0"},
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "a5edd4b28e4f6d042bb29b6fe5f8c7970a0f6485"},
		{Key: "vcs.time", Value: "2026-08-21T20:13:54Z"},
		{Key: "vcs.modified", Value: "true"},
	}
}

func TestBuildSettingsKeepsOnlyTheAllowList(t *testing.T) {
	got := buildSettings(&buildinfo.BuildInfo{Settings: realWorldSettings()})
	want := map[string]string{
		"CGO_ENABLED":  "0",
		"GOARCH":       "arm64",
		"GOARM64":      "v8.0",
		"-trimpath":    "true",
		"vcs":          "git",
		"vcs.revision": "a5edd4b28e4f6d042bb29b6fe5f8c7970a0f6485",
		"vcs.time":     "2026-08-21T20:13:54Z",
		"vcs.modified": "true",
	}
	if !maps.Equal(got, want) {
		t.Errorf("buildSettings = %v, want %v", got, want)
	}
}

// The three keys above that carry operator-written strings, named individually:
// -ldflags holds an internal build hostname here, -pgo an absolute path on the
// build machine, and GOOS is a constant for every process the agent can see.
// This is the test that fails if someone widens the list without meaning to.
func TestBuildSettingsDropsOperatorWrittenFlags(t *testing.T) {
	got := buildSettings(&buildinfo.BuildInfo{Settings: realWorldSettings()})
	for _, key := range []string{"-ldflags", "-pgo", "-gcflags", "-asmflags", "-tags", "-buildmode", "-compiler", "GOOS"} {
		if v, ok := got[key]; ok {
			t.Errorf("%s survived the allow-list with value %q", key, v)
		}
	}
}

// An allowed key whose value is longer than any real one is dropped whole,
// never truncated: a prefix of an unexpected string is still an unexpected
// string, and would ship looking like a legitimate value.
func TestBuildSettingsDropsOverlongValues(t *testing.T) {
	long := strings.Repeat("x", maxSettingValue+1)
	got := buildSettings(&buildinfo.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: long},
		{Key: "GOARCH", Value: "amd64"},
	}})
	if _, ok := got["vcs.revision"]; ok {
		t.Error("an overlong vcs.revision was kept")
	}
	if got["GOARCH"] != "amd64" {
		t.Error("dropping one setting must not drop the rest")
	}
}

// A binary with nothing allow-listed yields nil rather than an empty map, so the
// payload omits the field instead of carrying an empty object.
func TestBuildSettingsOfAnUnstampedBinaryIsNil(t *testing.T) {
	if got := buildSettings(&buildinfo.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "-ldflags", Value: "-s -w"},
	}}); got != nil {
		t.Errorf("buildSettings = %v, want nil", got)
	}
	if got := buildSettings(&buildinfo.BuildInfo{}); got != nil {
		t.Errorf("buildSettings of a settingless binary = %v, want nil", got)
	}
}

func TestHasPGO(t *testing.T) {
	cases := []struct {
		name     string
		settings []debug.BuildSetting
		want     bool
	}{
		{"pgo with profile path", []debug.BuildSetting{{Key: "-pgo", Value: "default.pgo"}}, true},
		{"pgo off", []debug.BuildSetting{{Key: "-pgo", Value: "off"}}, false},
		{"pgo empty", []debug.BuildSetting{{Key: "-pgo", Value: ""}}, false},
		{"no pgo setting", []debug.BuildSetting{{Key: "-trimpath", Value: "true"}}, false},
		{"no settings", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasPGO(&buildinfo.BuildInfo{Settings: tc.settings}); got != tc.want {
				t.Errorf("hasPGO = %v, want %v", got, tc.want)
			}
		})
	}
}
