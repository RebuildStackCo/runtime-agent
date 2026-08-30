package nodescan

import (
	"bufio"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The listening-socket reader (ADR 0056 §2). It answers where a pprof endpoint
// would be, from the process's own file descriptors rather than from a port
// sweep.
//
// Only rows in state 0A are parsed. Every other row is an accepted or outgoing
// connection, which is where peer addresses live, and it is discarded on the
// state field before any address is read — so no remote address is held at any
// moment, rather than held and dropped (CLAUDE.md invariant 4).

// tcpStateListen is TCP_LISTEN as /proc/net/tcp writes it.
const tcpStateListen = "0A"

// maxSocketBytes bounds one socket table read, and maxProcessFDs bounds the
// descriptor walk. A process past either is not one this reader can attribute
// ports for, and it reports none.
const (
	maxSocketBytes = 4 << 20
	maxProcessFDs  = 1 << 16
)

// ListeningPort is one TCP port a process accepts connections on.
type ListeningPort struct {
	Port int `json:"port"`
	// Loopback is true when the socket is bound to 127.0.0.0/8 or ::1, so
	// nothing outside the pod can reach it. The address itself is not kept: a
	// socket bound to the pod's own IP would carry that IP, and which of the
	// three classes an address falls in is the whole of what a finding needs.
	Loopback bool `json:"loopback,omitempty"`
}

// ReadListeningPorts returns the TCP ports process pid accepts on, in port
// order. An unreadable table or descriptor directory yields nothing.
//
// The ports are the process's own, not the pod's: every container in a pod
// shares one network namespace, so the socket table is shared, and a row is kept
// only when this process holds the descriptor. That is also what makes a
// host-network pod safe to read — the node's own listeners belong to other
// processes and are never attributed to a workload (ADR 0056 §2).
func ReadListeningPorts(procRoot string, pid int) []ListeningPort {
	byInode := map[uint64]ListeningPort{}
	for _, name := range []string{"tcp", "tcp6"} {
		readListenRows(filepath.Join(procRoot, strconv.Itoa(pid), "net", name), byInode)
	}
	if len(byInode) == 0 {
		return nil
	}
	owned := processSocketInodes(procRoot, pid)
	found := make([]ListeningPort, 0, len(byInode))
	for inode, lp := range byInode {
		if _, ok := owned[inode]; ok {
			found = append(found, lp)
		}
	}
	return MergeListeningPorts(found)
}

// MergeListeningPorts unions port lists into one, in port order.
//
// A port may appear more than once — IPv4 and IPv6 are separate sockets, and
// separate replicas report separately. It is reachable from outside if any of
// those bindings is, so loopback survives only when every one of them is
// loopback.
func MergeListeningPorts(lists ...[]ListeningPort) []ListeningPort {
	byPort := map[int]bool{}
	for _, list := range lists {
		for _, lp := range list {
			if prev, seen := byPort[lp.Port]; seen {
				byPort[lp.Port] = prev && lp.Loopback
				continue
			}
			byPort[lp.Port] = lp.Loopback
		}
	}
	if len(byPort) == 0 {
		return nil
	}
	out := make([]ListeningPort, 0, len(byPort))
	for port, loopback := range byPort {
		out = append(out, ListeningPort{Port: port, Loopback: loopback})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// readListenRows adds the listening sockets of one table to byInode.
func readListenRows(path string, byInode map[uint64]ListeningPort) {
	f, err := os.Open(path) // #nosec G304 -- procRoot is an operator-set flag; pid is a live process
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	parseListenRows(io.LimitReader(f, maxSocketBytes), byInode)
}

// parseListenRows keeps the listening rows of a /proc/net/tcp-shaped table.
func parseListenRows(r io.Reader, byInode map[uint64]ListeningPort) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// sl, local, rem, st, … , inode: a row shorter than this is the header
		// or a format this parser does not assume.
		if len(fields) < 10 || fields[3] != tcpStateListen {
			continue
		}
		port, loopback, ok := parseLocalAddress(fields[1])
		if !ok {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}
		byInode[inode] = ListeningPort{Port: port, Loopback: loopback}
	}
}

// parseLocalAddress splits a "HEXADDR:HEXPORT" local address into the port and
// whether the address is a loopback one. The address bytes leave this function
// as one bit.
//
// The kernel writes each 32-bit word of the address in host order, so on a
// little-endian machine the bytes of every word are reversed; IPv6 is four such
// words.
func parseLocalAddress(field string) (port int, loopback bool, ok bool) {
	addrHex, portHex, cut := strings.Cut(field, ":")
	if !cut {
		return 0, false, false
	}
	p, err := strconv.ParseUint(portHex, 16, 32)
	if err != nil || p == 0 || p > 65535 {
		return 0, false, false
	}
	raw, err := hex.DecodeString(addrHex)
	if err != nil || (len(raw) != net.IPv4len && len(raw) != net.IPv6len) {
		return 0, false, false
	}
	for word := 0; word+4 <= len(raw); word += 4 {
		raw[word], raw[word+3] = raw[word+3], raw[word]
		raw[word+1], raw[word+2] = raw[word+2], raw[word+1]
	}
	return int(p), net.IP(raw).IsLoopback(), true
}

// processSocketInodes returns the socket inodes process pid holds open. It is
// read only when the table showed a listening socket at all, so a process that
// accepts nothing never has its descriptors walked.
func processSocketInodes(procRoot string, pid int) map[uint64]struct{} {
	dir := filepath.Join(procRoot, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > maxProcessFDs {
		return nil
	}
	inodes := make(map[uint64]struct{})
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		if inode, ok := socketInode(target); ok {
			inodes[inode] = struct{}{}
		}
	}
	return inodes
}

// socketInode reads the inode out of a descriptor's "socket:[12345]" target.
// Any other target — a file, a pipe — is not a socket and yields nothing, so no
// path a process has open is ever examined further.
func socketInode(target string) (uint64, bool) {
	rest, ok := strings.CutPrefix(target, "socket:[")
	if !ok {
		return 0, false
	}
	digits, ok := strings.CutSuffix(rest, "]")
	if !ok {
		return 0, false
	}
	inode, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return inode, true
}
