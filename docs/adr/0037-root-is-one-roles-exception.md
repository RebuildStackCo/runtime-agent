# 0037. The image runs as uid 65532; root is one role's exception, asked for by name

Date: 2026-08-27

Status: Accepted
Amends: 0009 §4

Changes the image's default user and adds `runAsNonRoot` to the controller. The
node role keeps root and now says why. No change to what is collected, to any
payload, or to any RBAC rule.

## Context

The agent image was built on `gcr.io/distroless/static:latest` with no `USER`
line, so both roles ran as uid 0. The Dockerfile explained this as a
consequence of the node role's needs: it reads other containers'
`/proc/<pid>/exe`, which is root-owned.

That is one role's requirement stated as the image's default, and it carried to
a role that has no such requirement at all. The controller reads the Kubernetes
API, polls kubelets through the API server, and writes payload files to an
`emptyDir`. It mounts no host path, opens no other process's `/proc`, and holds
every API grant in the product — it is the component a security review looks at
first, and it was running as root for a reason belonging to a different
component.

The obvious question is whether the node needs root either. The kernel says it
does not: `__ptrace_may_access` admits either a matching uid or `CAP_SYS_PTRACE`
in the target's user namespace, and the node already holds that capability.

That reasoning is wrong in practice, and the measurement says so. On Kubernetes
1.36, one pod, `hostPID: true`, all capabilities dropped except `SYS_PTRACE`,
the only difference being the uid:

| | `readlink /proc/1/exe` |
|---|---|
| uid 0 | `/usr/lib/systemd/systemd` |
| uid 65532 | denied |

A capability granted to a non-root process does not survive `execve` unless it
is in the *ambient* set, and Kubernetes does not populate it. The limit is
neither the kernel's nor the agent's; it is what the container runtime interface
will hand over. `/proc/<pid>/cgroup` is world-readable and needs none of this —
root buys exactly one operation, reading the executable.

## Decision

**1. The image runs as uid 65532.** The base becomes
`gcr.io/distroless/static:nonroot` with an explicit `USER 65532:65532`. The safe
posture is the default, and privilege is something a workload asks for.

**2. The controller asserts `runAsNonRoot: true` and pins no uid.** The
assertion is the load-bearing half: kubelet refuses to start a container whose
image would run as root, so an image rebuilt without its `USER` line fails
visibly instead of quietly regaining privilege. The uid itself stays in the
image, because repeating it in the chart would be the same fact in two places.

**3. The controller sets no `fsGroup`, and that is a measured decision rather
than an omission.** It looks required: the spool holds files a non-root process
must create, and an `emptyDir` is created by kubelet owned by root. It is not
required, because kubelet also creates it world-writable — `drwxrwxrwx`,
observed — so the image's uid writes there unaided. The full policy suite passes
against a non-root controller with no `fsGroup` at all.

This was very nearly shipped the other way: `fsGroup: 65532` was written first,
on the strength of the argument above, and only the negative run showed that
removing it changed nothing. A setting that does nothing is worse than absent —
it reads as a requirement and is a belief nobody checked.

An `fsGroup` becomes necessary the moment the spool path is backed by something
other than an `emptyDir`. The chart offers no such option (ADR 0026), so this is
a note for whoever adds one, not a knob held in reserve.

**4. The node role overrides back to `runAsUser: 0`, in the chart, with the
measurement recorded next to it.** Not in the image, where it would apply to
both roles again; not silently, by inheriting a root default. The exception is
declared at the point that needs it, so a reader of the DaemonSet sees both that
it is root and why.

**5. `docs/security.md` states the measured reason.** It previously said the
node "runs as UID 0 (needed to match the credentials of the root processes it
reads)", which describes one branch of the kernel's check and implies the other
branch was never available. The other branch exists and is unreachable through
Kubernetes; that is a different claim and a more useful one, because it names
what would have to change for the node to drop root.

**6. Both halves are asserted against the rendered chart.** The controller must
declare `runAsNonRoot`, must pin no uid, and must carry no `fsGroup`; the node
must ask for root explicitly. The second assertion is the one that ages well: it
turns a node that silently inherits root from a changed image into a failure
rather than a continuation.

## Consequences

**Easier.** The component holding every API grant no longer runs as root, and
the product has exactly one privileged role with a written reason. A reviewer
reading the DaemonSet finds the exception and its justification in the same
place; a reviewer reading the Deployment finds no exception at all.

**Harder / given up.** Anything sharing the spool must now run as the same uid:
the payloads are `0600` and owned by the agent, so the e2e's shell sidecar runs
as 65532 and says why. A future volume for the spool that is not an `emptyDir`
inherits a question the `emptyDir` answers for free.

Dropping root on the node stays out of reach. It would need file capabilities
on the binary (`setcap cap_sys_ptrace+ep`), which means a `setcap` step in a
distroless build with no libcap, for one role already bounded by zero RBAC, no
API token and no writable host path. Recorded so it stays a decision.

**Not changed.** Nothing about collection, payloads, filtering or RBAC. The node
reads exactly what it read before, with the same single capability.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
