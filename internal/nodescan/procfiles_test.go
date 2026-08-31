package nodescan

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

const realWorldLimits = `Limit                     Soft Limit           Hard Limit           Units     
Max cpu time              unlimited            unlimited            seconds   
Max file size             unlimited            unlimited            bytes     
Max processes             unlimited            unlimited            processes 
Max open files            65536                65536                files     
Max locked memory         8388608              8388608              bytes     
`

// testPID is the process every fixture in this file describes.
const testPID = 42

// writeProcessFiles builds a /proc/<testPID> with a descriptor directory of n
// entries and the limits table above.
func writeProcessFiles(t *testing.T, root string, n int, limits string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(testPID))
	fd := filepath.Join(dir, "fd")
	if err := os.MkdirAll(fd, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err := os.WriteFile(filepath.Join(fd, strconv.Itoa(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if limits != "" {
		if err := os.WriteFile(filepath.Join(dir, "limits"), []byte(limits), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Both halves come from the same process at the same moment, so the headroom
// between them is a fact rather than a subtraction across two readings.
func TestOpenDescriptorsAndTheirCeilingComeTogether(t *testing.T) {
	root := t.TempDir()
	writeProcessFiles(t, root, 7, realWorldLimits)

	got := ReadProcessFiles(root, testPID)
	if got.Open != 7 {
		t.Errorf("open = %d, want 7", got.Open)
	}
	if got.Limit != 65536 {
		t.Errorf("limit = %d, want 65536 (the soft column, which is the one in force)", got.Limit)
	}
}

// "unlimited" is not a ceiling, and reporting it as a very large number would
// read as one — a headroom of billions is a claim, and a false one.
func TestAnUnlimitedCeilingIsNoCeiling(t *testing.T) {
	root := t.TempDir()
	writeProcessFiles(t, root, 1, "Max open files            unlimited            unlimited            files\n")
	if got := ReadProcessFiles(root, testPID); got.Limit != 0 {
		t.Errorf("limit = %d, want 0 for an unlimited ceiling", got.Limit)
	}
}

// A row whose name merely starts the same must not be read as the one wanted.
func TestOnlyTheDescriptorRowIsRead(t *testing.T) {
	root := t.TempDir()
	writeProcessFiles(t, root, 1, "Max file size             1024                 2048                 bytes\n")
	if got := ReadProcessFiles(root, testPID); got.Limit != 0 {
		t.Errorf("limit = %d, want 0: no descriptor row is present", got.Limit)
	}
}

// A process that exited mid-pass, or a kernel that does not expose the files.
func TestMissingDescriptorFilesAreTheZeroValue(t *testing.T) {
	if got := ReadProcessFiles(t.TempDir(), 1); got != (ProcessFiles{}) {
		t.Errorf("missing files = %+v, want the zero value", got)
	}
}

// The count is bounded, for the reason the socket reader's is: past the bound
// the answer is "many", and a process holding a million descriptors must not
// turn one scan pass into a million reads.
func TestTheDescriptorCountIsBounded(t *testing.T) {
	root := t.TempDir()
	writeProcessFiles(t, root, 40, realWorldLimits)
	if got := ReadProcessFiles(root, testPID); got.Open > maxProcessFDs {
		t.Errorf("open = %d, above the bound %d", got.Open, maxProcessFDs)
	}
}
