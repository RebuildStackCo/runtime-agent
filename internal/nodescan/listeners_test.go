package nodescan

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A real /proc/<pid>/net/tcp, trimmed to one of each row this parser has to
// tell apart. The established rows are the point: their peer addresses are the
// thing this reader must never carry, and they are dropped on the state field
// before an address is parsed.
const realWorldTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 65532        0 41215 1 0000000000000000 100 0 0 10 0
   1: 0100007F:17A4 00000000:0000 0A 00000000:00000000 00:00000000 00000000 65532        0 41216 1 0000000000000000 100 0 0 10 0
   2: 0A80000C:1F90 0B80000C:D431 01 00000000:00000000 02:00000B48 00000000 65532        0 41290 3 0000000000000000 20 4 30 10 -1
   3: 0A80000C:C8F2 0104040A:0050 06 00000000:00000000 03:000004D4 00000000     0        0 0 3 0000000000000000
`

// The IPv6 table of the same process: the same wildcard port bound a second
// time, which is what makes port de-duplication necessary rather than tidy.
const realWorldTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 65532        0 41217 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:1ADB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 65532        0 41218 1 0000000000000000 100 0 0 10 0
`

func TestOnlyListeningRowsAreParsed(t *testing.T) {
	got := map[uint64]ListeningPort{}
	parseListenRows(strings.NewReader(realWorldTCP), got)

	want := map[uint64]ListeningPort{
		41215: {Port: 8080},
		41216: {Port: 6052, Loopback: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listening rows = %v, want %v (established rows must not appear)", got, want)
	}
}

func TestIPv6AddressWordsAreUnreversed(t *testing.T) {
	got := map[uint64]ListeningPort{}
	parseListenRows(strings.NewReader(realWorldTCP6), got)

	want := map[uint64]ListeningPort{
		41217: {Port: 8080},
		41218: {Port: 6875, Loopback: true}, // ::1
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listening rows = %v, want %v", got, want)
	}
}

func TestLocalAddressIsRejectedRatherThanGuessed(t *testing.T) {
	cases := []struct {
		name  string
		field string
	}{
		{"no port", "00000000"},
		{"port zero", "00000000:0000"},
		{"port past the range", "00000000:1FFFF"},
		{"address of no known length", "000000:1F90"},
		{"address that is not hex", "zzzzzzzz:1F90"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := parseLocalAddress(tc.field); ok {
				t.Errorf("parsed %q, want a refusal", tc.field)
			}
		})
	}
}

// The descriptor check is what scopes the answer to this process. Without it a
// pod's sidecar ports would be reported as the application's, and a
// host-network pod would be reported as serving the whole node.
func TestOnlyTheProcessOwnSocketsAreReported(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "42")
	writeFixture(t, filepath.Join(proc, "net", "tcp"), realWorldTCP)
	writeFixture(t, filepath.Join(proc, "net", "tcp6"), realWorldTCP6)

	// The process holds the two wildcard :8080 sockets and a regular file; the
	// loopback ones belong to something else in the same network namespace.
	linkFixture(t, filepath.Join(proc, "fd", "3"), "socket:[41215]")
	linkFixture(t, filepath.Join(proc, "fd", "4"), "socket:[41217]")
	linkFixture(t, filepath.Join(proc, "fd", "5"), "/etc/passwd")

	got := ReadListeningPorts(root, 42)
	want := []ListeningPort{{Port: 8080}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ports = %v, want %v", got, want)
	}
}

// A port bound on both families is one port, and it is reachable from outside if
// either binding is.
func TestPortsMergeAcrossFamiliesAndReplicas(t *testing.T) {
	got := MergeListeningPorts(
		[]ListeningPort{{Port: 8080, Loopback: true}, {Port: 6060, Loopback: true}},
		[]ListeningPort{{Port: 8080}, {Port: 9090}},
	)
	want := []ListeningPort{{Port: 6060, Loopback: true}, {Port: 8080}, {Port: 9090}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged = %v, want %v", got, want)
	}
}

func TestNoSocketsMeansNoDescriptorWalk(t *testing.T) {
	root := t.TempDir()
	// No net/ tables at all, and an fd directory that would panic nothing but
	// must not be reached for an answer.
	linkFixture(t, filepath.Join(root, "42", "fd", "3"), "socket:[41215]")
	if got := ReadListeningPorts(root, 42); got != nil {
		t.Errorf("ports = %v, want none", got)
	}
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func linkFixture(t *testing.T, path, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}
