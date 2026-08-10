package nodescan

import (
	"debug/buildinfo"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
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

	root := stageProc(t, []procEntry{
		{pid: 100, exe: keptBin, cgroup: keptCgroup},
		{pid: 200, exe: infraBin, cgroup: "0::/kubepods/besteffort/pod0000/x"},
		{pid: 300, exe: "this is not an executable at all", rawFile: true}, // unreadable
		{pid: 400}, // no exe: kernel thread / gone -> skipped
	}, []string{"meminfo", "self"})

	s := NewScanner(root, NewModuleFilter(DefaultInfraModulePrefixes))
	res, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	want := Counters{ProcessesScanned: 4, GoFound: 1, FilteredInfra: 1, Unreadable: 1}
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

func TestDependencyPaths(t *testing.T) {
	info := &buildinfo.BuildInfo{
		Deps: []*debug.Module{
			{Path: "example.com/a", Version: "v1.0.0"},
			nil, // the toolchain can leave holes; must be skipped, not panic
			{Path: "example.com/b", Version: "v2.3.4"},
		},
	}
	got := dependencyPaths(info)
	want := []string{"example.com/a", "example.com/b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("dependencyPaths = %v, want %v", got, want)
	}
	if dependencyPaths(&buildinfo.BuildInfo{}) != nil {
		t.Error("dependencyPaths of a depless binary should be nil")
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
