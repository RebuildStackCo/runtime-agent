# 0009. The node role: an on-node Go-binary scanner with zero API access

Date: 2026-08-10
Status: Accepted
Amended by: 0010, 0011, 0015, 0022

## Context

The controller learns a great deal about workloads from the Kubernetes API
(ADR 0006), but the API cannot say which of those workloads are Go programs,
what Go version built them, which module they are, what they depend on, or
whether profile-guided optimization was applied. That information is embedded
in the executables themselves (`debug/buildinfo`) and can only be read on the
node where the process runs. It is directly useful to the product: the Go
version and module inventory drive "which of your services would benefit from
a newer toolchain / from PGO" findings, in resource units, with no code
uploaded.

`security.md` §2 already anticipates two node functions: eBPF CPU profiling and
Go-binary detection. These have very different privilege needs. eBPF profiling
loads programs and samples the CPU (`CAP_BPF`, `CAP_PERFMON`, kernel 5.8+).
Reading build info needs none of that — it is an ordinary file read of
`/proc/<pid>/exe`. Coupling the two would force every cluster that wants a Go
inventory to also grant eBPF privileges. We want the inventory on its own, at
the lowest privilege that can produce it.

Two constraints shape the design:

- **The node component must not become a second door to the API server.** The
  controller's API access is audited in one place (ADR 0008, `security.md` §4).
  A node component with even read RBAC would widen that surface across every
  node. The node role should hold *no* Kubernetes permissions at all.
- **Filter early (CLAUDE.md invariant 4).** Cluster infrastructure — the
  control plane, the runtime, the CNI, the observability stack — is not the
  customer's cost to analyze. Its identities must never be recorded, only
  counted.

An empirical fact settled the capability question. Container processes are
non-dumpable, so the kernel's `ptrace_may_access` check blocks reading another
container's `/proc/<pid>/exe` even for a same-UID root reader — verified in
kind: without the capability the scanner reads only its own executable and
counts every other process as unreadable; with it, every readable Go binary is
identified. This resolves the `CAP_SYS_PTRACE` "TBD" left open in `security.md`
§7: the capability is required for build-info extraction, and it is distinct
from the eBPF capabilities.

## Decision

1. **One binary, two roles.** The agent selects its role from the first
   argument: `agent node` runs the scanner; anything else (including no
   argument) runs the controller, so existing invocations are unchanged. The
   node role constructs **no** Kubernetes client — the code path that reaches
   the API server does not exist in that role.

2. **What the node role reads, and only that.** As a DaemonSet with
   `hostPID: true`, for each process under `/proc` it reads:
   - `/proc/<pid>/exe` → `debug/buildinfo`: Go version, main module path,
     dependency module paths, and build settings (including `-pgo`).
   - `/proc/<pid>/cgroup` → the pod UID and container ID the kubelet encodes
     into the cgroup path (both cgroupfs and systemd driver shapes).

   It opens no sockets, reads no other `/proc` entries, and mounts the host
   `/proc` read-only.

3. **Privileges: the minimum that works.**
   - `hostPID` — to see node processes and open their `/proc/<pid>/exe`.
   - `CAP_SYS_PTRACE` — the **only** added capability; required by the ptrace
     access check above. **No `CAP_BPF`/`CAP_PERFMON`** — the scanner loads no
     eBPF and does no sampling.
   - Runs as UID 0 to match the credentials of the (root) processes it reads;
     compensated by `readOnlyRootFilesystem`, `seccompProfile: RuntimeDefault`,
     `privileged: false`, `allowPrivilegeEscalation: false`, and dropping every
     capability except `SYS_PTRACE`.

4. **Zero RBAC, no token.** The node role's ServiceAccount is bound to nothing:
   no Role, RoleBinding, ClusterRole, or ClusterRoleBinding is created for it
   anywhere, and `automountServiceAccountToken: false` keeps its token out of
   the container. Its inability to reach the API is a property of the absence
   of any grant, enforced by the API server, not by agent configuration.

5. **Filter on the node.** A binary's main module path is matched against a
   built-in infrastructure deny-list (`k8s.io/`, `sigs.k8s.io/`,
   `github.com/containerd/`, `github.com/coredns/`, `go.etcd.io/`,
   `github.com/prometheus/`, `github.com/grafana/`, … and this agent's own
   `github.com/RebuildStackCo/`). Infrastructure is dropped before any record
   is formed. Only four aggregate counters describe what was not kept:
   processes scanned, Go binaries found, filtered as infrastructure, and
   unreadable (a real executable with no recoverable Go build info — a non-Go
   program, or a Go binary whose build info was removed). Pod UID, container
   ID, and module path are retained **only** for kept (non-infrastructure)
   binaries.

6. **Scope of this slice.** The result is written to the node role's structured
   log. Delivery to the controller for aggregation is a later slice. The
   one-way boundary is unchanged in either case: the node role talks only to
   the controller (never externally, `security.md` §5), and here it talks to
   no one.

## Consequences

Easier:

- A Go-version and module inventory becomes available at the lowest privilege
  that can produce it: a node component with **no Kubernetes identity**, no
  eBPF, and no external egress.
- The `CAP_SYS_PTRACE` question in `security.md` §7 is now answered rather than
  deferred, and the capability is scoped to exactly the build-info reader.
- Role selection is a one-argument change; the controller path is untouched,
  so nothing about ADR 0001/0006/0008 shifts.

Harder, or given up:

- `hostPID` lets the scanner observe every process on the node, including pods
  outside any namespace filter — inherent to node-level scanning
  (`security.md` §10.2). The on-node module filter is the mitigation: infra and
  unattributed binaries never leave as anything but a count.
- `debug/buildinfo` is best-effort. A binary whose build info was stripped is
  counted "unreadable," not identified; a genuinely non-Go program is
  indistinguishable from that case and lands in the same aggregate. The scanner
  reports the ambiguity as a number instead of guessing.
- Running as UID 0 is a real privilege. It is bounded by the read-only root
  filesystem, the dropped capabilities, seccomp, no privilege escalation, and —
  most of all — the total absence of API access.
- The node role adds no persistent state; ADR 0003's loss-harmless property is
  unaffected (there is nothing to lose).
