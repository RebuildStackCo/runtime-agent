package nodescan

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A real rollup, trimmed. Its first line is a mapping's address range and
// permissions, and on some kernels a path — the identity this parser must not
// pick up, and the reason it takes named keys only.
const realWorldSmapsRollup = `55a3f0000000-7ffd8c7f2000 ---p 00000000 00:00 0                          [rollup]
Rss:              402136 kB
Pss:              340892 kB
Pss_Dirty:        331904 kB
Shared_Clean:      61440 kB
Shared_Dirty:          0 kB
Private_Clean:      8792 kB
Private_Dirty:    331904 kB
Referenced:       402136 kB
Anonymous:        331904 kB
`

func TestProcessMemoryKeepsTheTwoSharingNumbers(t *testing.T) {
	got := parseProcessMemory(strings.NewReader(realWorldSmapsRollup))
	if want := int64(340892) * 1024; got.PSSBytes != want {
		t.Errorf("pss = %d, want %d", got.PSSBytes, want)
	}
	if want := int64(331904) * 1024; got.PrivateDirtyBytes != want {
		t.Errorf("private dirty = %d, want %d", got.PrivateDirtyBytes, want)
	}
}

// Pss_Dirty sits directly under Pss and Private_Clean directly above
// Private_Dirty; a prefix match rather than an exact key would take the wrong
// one and be wrong by a plausible amount, which is the worst kind of wrong.
func TestNeighbouringKeysAreNotMistakenForTheOnesRead(t *testing.T) {
	got := parseProcessMemory(strings.NewReader(realWorldSmapsRollup))
	for _, wrong := range []int64{402136, 61440, 8792} {
		if got.PSSBytes == wrong*1024 || got.PrivateDirtyBytes == wrong*1024 {
			t.Errorf("a neighbouring key was read as one of the two wanted (%d kB)", wrong)
		}
	}
}

// A kernel before 4.14 has no rollup at all, and a process can exit mid-pass.
// Both are the zero value, not an error: the record's process count already
// says how many processes stood behind the numbers.
func TestAMissingRollupIsTheZeroValue(t *testing.T) {
	if got := ReadProcessMemory(t.TempDir(), 1); got != (ProcessMemory{}) {
		t.Errorf("missing rollup = %+v, want the zero value", got)
	}
}

func TestReadProcessMemoryReadsTheFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, strconv.Itoa(42))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "smaps_rollup"), []byte(realWorldSmapsRollup), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ReadProcessMemory(root, 42); got.PSSBytes != 340892*1024 {
		t.Errorf("pss = %d, want %d", got.PSSBytes, 340892*1024)
	}
}
