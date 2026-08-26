# 0039. A stated limit is a measured limit, or it is marked

Date: 2026-08-26

Status: Accepted

Amends: 0009 §3, 0010 §Consequences, 0011 §Consequences, 0015 §1, 0025 §5

Corrects six security claims that were written as measured properties and are
not. Changes no code, no payload, no RBAC rule and no chart template: every
correction here is a claim moving to match an implementation that already
behaves this way.

## Context

A five-dimension audit — privilege surface, the node→controller channel, data
egress, supply chain, runtime robustness — read the code against
`docs/security.md` and the decision records. It found no way for an outsider to
enter a cluster and no over-granted RBAC rule: every resource in the ClusterRole
has a consumer and every consumer has a grant. What it found instead was one
repeated shape, in every dimension it looked at:

> the code is careful; the documents are more confident than the code.

Six claims are wrong, and they are wrong in the direction that matters — each
one tells a reviewer that a boundary is tighter than it is. Listed with what is
actually true:

**1. The node's privilege is bounded by its compensating controls.** ADR 0009
§Consequences: "Running as UID 0 is a real privilege. It is bounded by the
read-only root filesystem, the dropped capabilities, seccomp, no privilege
escalation, and — most of all — the total absence of API access."
`docs/security.md` §7.1 sharpened this to "Root buys exactly one operation,
reading `/proc/<pid>/exe`."

Every one of those controls is real and not one of them constrains the actual
primitive. `hostPID` plus `CAP_SYS_PTRACE` plus uid 0 satisfies
`ptrace_may_access(PTRACE_MODE_READ)` against every process on the node, and the
`/proc/<pid>/root` magic symlink then resolves in the *target's* mount
namespace. That is read access to every other container's filesystem on that
node, including each one's `/var/run/secrets/kubernetes.io/serviceaccount/token`
and every mounted Secret volume — so the node role does read other workloads'
secrets, just not through the API server, which is the one place §9 looked. It
is also `/proc/<pid>/mem` and `PTRACE_ATTACH` against host processes, and the
read-only flag on the `/host/proc` mount buys nothing against any of it: with
`hostPID` the container's *own* `/proc` is mounted read-write by the runtime and
shows the identical process table. The `hostPath` is redundant with `hostPID`.

The default `tolerations: [{operator: Exists}]` means this lands on
control-plane nodes too. On a self-managed cluster that is where the API
server, etcd and the service-account signing key live as processes and files, so
the same primitive reaches them; on a managed control plane they are not in the
customer's node pool and it does not. The document must say which, because
"control-plane node" means different things on the two.

**2. The agent cannot modify your workloads.** `docs/security.md` §1 principle 1
and §9. The controller holds `get` on `nodes/proxy`, and the kubelet registers
**GET** routes for `/exec/{ns}/{pod}/{container}`, `/attach/…` and
`/portForward/…`. So §9's "No `pods/exec`, `pods/attach`, `pods/portforward`" is
true about resource names and false about capability, and §4's honest disclosure
stopped at "including node logs" — one endpoint short of the one that matters,
and short of `/containerLogs`, which routinely carries credentials.

**3. A NetworkPolicy restricts who may reach the receiver port.** Stated as an
existing compensating control in ADR 0010 §Consequences, ADR 0011
§Consequences, ADR 0015 §1, and a code comment in `internal/nodeintake/server.go`.
No NetworkPolicy exists in the chart or anywhere else. `docs/security.md` alone
is correct here — it marks the policy **[planned]** in both places it appears,
which is why the error survived: the document that reviewers read was right, and
the documents that justify it were wrong.

**4. A stack trace never leaves unless its module path is on the allow-list.**
`docs/security.md` §8, and ADR 0025 §5's finding that the symbol allow-list is
the load-bearing control that survives a hostile controller. Two frames get past
it. `SymbolFilter.keep` returns early for any package whose first path segment
has no dot — which is the workload's own `main` package, and any module whose
`go.mod` says `module acmecorp` rather than `module github.com/acme/…`. `main`
is where a service's request handlers, business functions and job names live, so
the exemption admits the most identifying frames in the profile under a comment
asserting that they "carry no structure of your code."

Separately, `keep` classifies a frame on its `Kind` and `Function` and never
looks at its `File`, which is carried alongside and written into the shipped
pprof as `Function.Filename`. A binary built without `-trimpath` — the default —
therefore ships the build machine's absolute paths, the internal VCS hostname
and every source filename and line number of the hot path. This is the exact
class of string ADR 0019 excludes `-ldflags`, `-gcflags` and `-tags` for, in the
scanner, one package away.

**5. A pod the scanner may not open is not profiled either.** `docs/security.md`
§7.2. True of what is shipped and false of what is captured: the profiler
attaches node-wide with no PID or cgroup restriction, and every symbolized frame
— function name and source path — enters the node's buffer before any scope or
target filter runs. The frames of excluded namespaces, and of host processes
including the kubelet, transit the agent. §10.2 discloses this class of exposure
honestly; §7.2 reads as though the earlier gate applies to capture. For this
path CLAUDE.md's ordering — "not collected" beats "collected and discarded" — is
not met, and the scanner's genuine achievement of it (cgroup checked before the
executable is opened) made the weaker path look like the stronger one.

**6. Smaller claims that were simply never revisited.** The `ebpf` profile mounts
all of host `/sys/kernel`, not `/sys/kernel/btf`; the template says why and the
security document was not updated. §8 describes raw samples living in a "24–48h
ring buffer on an ephemeral `emptyDir` volume" — the DaemonSet has no `emptyDir`
and the buffer is memory-only, so the document describes a persistence surface
that does not exist. Node *names* ship in three payloads and on EKS encode the
node's private address, under a §8 promise that addresses are not read. Image
references ship unbounded and carry registry hostnames and cloud account
identifiers. `workload_policy` names PersistentVolumeClaims, and a StatefulSet's
claim name contains its pod's name, in a row marked "Names pods? no".

## Decision

**1. `docs/security.md` states what was measured, and marks what was not.** The
existing **[planned]** convention already carries "written but not built"; what
this ADR adds is that a claim about a *limit* — what an attacker cannot do, what
a control bounds — is either something someone checked or it does not appear.
The six above are rewritten to the measured version, including the parts that
are less flattering than what they replace.

**2. The node role's blast radius is stated as node compromise.** §7.1 says that
a compromise of the node container is a compromise of that node and of every
credential mounted on it, and that on a control-plane node it reaches the
cluster's signing keys. The compensating controls stay in the document, listed
as what they actually do — they bound what the agent's own code can do by
accident, not what code running in that container could do on purpose. The
privilege itself is unchanged and still justified: ADR 0037 measured that
Kubernetes does not populate the ambient capability set, so the scanner cannot
read `/proc/<pid>/exe` without root.

This is the correction that will be least comfortable to a reader, and it is the
one most worth making. An operator deciding whether to install the DaemonSet is
deciding to trust this agent's image with the node, and they should be deciding
that with the real question in front of them.

**3. `nodes/proxy` is disclosed as kubelet exec.** §4's row and §9's bullet name
`/exec`, `/attach`, `/portForward` and `/containerLogs` over GET. §1's
"read-only" principle keeps its first clause — the agent holds no write verb —
and loses the inference drawn from it, that the agent therefore cannot modify a
workload. What continues to bound this is not the grant: it is that all kubelet
access lives in one poll loop calling exactly two stats paths, which is an
auditable property of the code and is stated as one.

**4. The NetworkPolicy is marked [planned] wherever it is claimed.** In the ADRs
by this amendment, and in the code comment by editing it. Marking it is the
whole of this decision — building it belongs to the slice that adds node
identity, because the two are one boundary and shipping half of it would invite
the same over-reading this ADR exists to end.

**5. The symbol allow-list's bound is stated as it is, and the code moves
next.** §8 and the §7.2 table say that the workload's own `main` package is kept
unconditionally and that source file paths and line numbers ship. The allow-list
row in the "stops a hostile controller" table drops to a qualified yes with the
exemption named. ADR 0025 §5's conclusion is narrowed rather than reversed: the
allow-list is still the load-bearing control of that path, and it currently has
two holes in it. Closing them is the next slice and will produce its own ADR;
this one refuses to let the gap sit undocumented until then.

**6. Capture-time exposure is separated from egress-time filtering.** §7.2 states
that the profiler samples the whole node and that filtering happens after, with a
pointer to §10.2 rather than a restatement. The "filter early" principle in §1
gains one sentence saying which paths achieve it — the scanner does, by checking
the cgroup before opening an executable; the profiler does not, by construction
of node-wide perf sampling.

**7. No claim in this document rests on a control the chart does not render.**
Every compensating control §5 and §7 name is one an operator can read out of
`helm template` output, or it is marked. That rule is what all six failures have
in common, and it is cheaper to state once than to rediscover per claim.

## Consequences

**Easier.** A reviewer gets one document whose claims survive being checked. The
two corrections that change an operator's decision — what installing the
DaemonSet costs, and what `nodes/proxy` grants — are now in front of them at
install time rather than discoverable by audit. The three known code gaps
(NetworkPolicy, node identity, profile filtering) are written down as gaps, so
the slices that close them have a definition of done that is not a memory.

**Harder / given up.** The document is less flattering, and §7.1 in particular
now reads as a serious ask, because it is one. Some operators will decline the
`inventory` and `ebpf` profiles on the strength of it. That is the correct
outcome of an accurate disclosure and not a cost to be minimized: the alternative
is that they install on the strength of an inaccurate one.

Marking the NetworkPolicy rather than building it leaves the receiver reachable
by every pod in the cluster for one more slice, with the token in cleartext on
that path. The audit's judgement, recorded here so the sequencing is not
mistaken for a priority: an unauthenticated caller still cannot get past
signature, audience and the pinned subject, so what the missing policy costs is
defence in depth, and it is worth less than shipping it together with the node
identity that decides what an authenticated caller may say.

**Not changed.** No code, no payload, no RBAC rule, no chart template, no
default. The agent collects exactly what it collected before this ADR; what
changes is what the documents say it can reach. In particular the node's
`tolerations: [{operator: Exists}]` default stays — narrowing it would silently
reduce coverage, which is a decision an operator should make and not one to
smuggle in under a documentation change. §7.1 states what the default implies so
that the decision is available to them.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
