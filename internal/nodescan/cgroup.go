package nodescan

import (
	"regexp"
	"strings"
)

// PodBinding ties a scanned process to the pod and container it runs in. Both
// identifiers are derived from the process's own cgroup path, not from any
// Kubernetes API call: the node role holds no client (ADR 0009). Empty fields
// mean the path did not carry that identifier — a process outside the kubepods
// hierarchy (a host daemon) has neither.
type PodBinding struct {
	// PodUID is the Kubernetes pod UID, normalized to canonical dashed form.
	PodUID string
	// ContainerID is the runtime container ID (the 64-hex digest), without the
	// runtime prefix (containerd/crio/docker) the cgroup path may carry.
	ContainerID string
}

// The kubelet encodes the pod UID into the cgroup path, in two shapes:
//
//	cgroupfs: /kubepods/burstable/pod<uid>/<container-id>
//	systemd:  /kubepods.slice/…/kubepods-burstable-pod<uid>.slice/cri-containerd-<id>.scope
//
// Under systemd the UID's dashes become underscores; both are matched and
// normalized back to the form the API server would report.
var (
	podUIDPattern = regexp.MustCompile(`pod([0-9a-fA-F]{8}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{12})`)
	// A container ID is a 64-character hex digest. It may be bare (cgroupfs) or
	// wrapped as <runtime>-<id>.scope (systemd). We take the last such digest in
	// the path — the container segment is always the leaf.
	containerIDPattern = regexp.MustCompile(`[0-9a-fA-F]{64}`)
)

// parseCgroup extracts the pod UID and container ID from the contents of a
// process's /proc/<pid>/cgroup file. It tolerates both cgroup v1 (many lines,
// one hierarchy each) and cgroup v2 (a single "0::<path>" line) by scanning
// every line's path portion. A process not managed by the kubelet yields the
// zero PodBinding.
func parseCgroup(content string) PodBinding {
	var b PodBinding
	for line := range strings.SplitSeq(content, "\n") {
		// cgroup lines are "hierarchy-ID:controllers:path"; the path is
		// everything after the second colon (paths never contain colons).
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		path := parts[2]
		if b.PodUID == "" {
			if m := podUIDPattern.FindStringSubmatch(path); m != nil {
				b.PodUID = strings.ReplaceAll(m[1], "_", "-")
			}
		}
		// Take the last hex digest on the deepest path seen: the container
		// segment is the leaf, below the pod segment.
		if all := containerIDPattern.FindAllString(path, -1); len(all) > 0 {
			b.ContainerID = strings.ToLower(all[len(all)-1])
		}
	}
	return b
}
