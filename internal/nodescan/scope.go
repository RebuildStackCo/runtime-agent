package nodescan

// Scope is the set of pods the node is permitted to scan: the pod UIDs on this
// node that passed the controller's namespace filters and opt-out annotations.
//
// The node cannot compute this itself. It has zero Kubernetes API access (ADR
// 0009) and reads only /proc, where a process resolves to a pod UID and a
// container ID through its cgroup — never to a namespace. So the eligible set
// arrives from the controller, which holds the filters, exactly as the profiling
// targets do (ADR 0011 §3).
//
// The zero Scope admits nothing. That is deliberate: a node that has not
// obtained a scope must not scan, because "the process is in a namespace you
// allow-listed" is a claim it cannot make on its own, and docs/security.md §10.2
// promises customers that node-level samples outside their filters are dropped
// on the node before transport. Failing closed costs one scan pass, which the
// next pass recovers (loss-harmless, ADR 0003); failing open would break a
// published promise silently.
type Scope struct {
	uids map[string]struct{}
}

// NewScope builds a scope admitting exactly the given pod UIDs.
func NewScope(podUIDs []string) Scope {
	uids := make(map[string]struct{}, len(podUIDs))
	for _, uid := range podUIDs {
		if uid != "" {
			uids[uid] = struct{}{}
		}
	}
	return Scope{uids: uids}
}

// DenyAll is the scope of a node that has no eligible set: it admits nothing.
// It is what the node uses when the controller could not be reached.
func DenyAll() Scope {
	return Scope{}
}

// Admits reports whether a process bound to podUID may be scanned. A process
// with no pod UID — a host process outside the kubepods hierarchy, such as the
// kubelet or a systemd-managed daemon — is never admitted: it belongs to no
// namespace, so no namespace filter can permit it.
func (s Scope) Admits(podUID string) bool {
	if podUID == "" {
		return false
	}
	_, ok := s.uids[podUID]
	return ok
}

// Size is how many pods the scope admits, for the caller's log line. It is an
// aggregate count; the UIDs themselves are never logged.
func (s Scope) Size() int { return len(s.uids) }
