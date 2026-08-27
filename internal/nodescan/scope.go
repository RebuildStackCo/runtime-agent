package nodescan

// Scope is the set of pods the node is permitted to scan: the pod UIDs on this
// node that passed the controller's namespace filters and opt-out annotations.
//
// The node cannot compute it: zero API access (ADR 0009), and /proc resolves a
// process to a pod UID but never to a namespace. The set arrives from the
// controller, as profiling targets do (ADR 0011 §3). The zero Scope admits
// nothing: failing closed costs one pass (ADR 0003), failing open would break
// security.md §10.2 silently.
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
