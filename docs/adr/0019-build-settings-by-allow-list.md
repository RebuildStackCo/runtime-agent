# 0019. Build settings ship by allow-list, and the payload that carries them is named for the build

Date: 2026-08-22
Status: Accepted
Amends: 0017, 0018

Extends [ADR 0017](0017-build-facts-keyed-by-digest.md): the payload it
introduced as `go_dependencies` is renamed `go_build` and gains a `settings`
object. ADR 0017's decisions — write-once, keyed by image digest, no sequence,
no capture time — are unchanged and now cover the settings too.

## Context

The scanner has always read the whole build-settings block of every kept binary
and thrown all but one bit of it away. `hasPGO` looks for a `-pgo` key and
returns a boolean; everything else the toolchain recorded is discarded on the
node.

What is discarded decides whether a Go workload can be advised at all.
`GOAMD64` says which microarchitecture level the binary may use, and a `v1`
binary on a `v3` fleet is leaving instructions unused. `CGO_ENABLED` decides
whether the binary is statically linkable at all. `-race` in production costs
multiples of CPU and memory and is a finding on its own. `vcs.revision` ties a
running binary to the commit it was built from, and `vcs.modified` says the
build came from a dirty tree.

Two things make this different from every other fact the agent collects.

**The values are written by people, not by the toolchain.** Everywhere else the
agent reads a Kubernetes field or a kernel counter. Build settings include
`-ldflags`, and a real `-ldflags` from this repository's own image looks like
`-X main.version=a5edd4b-dirty` — with `-X main.buildHost=ci-07.internal.acme.corp`
being an equally ordinary sight. `-pgo` holds an absolute path on the build
machine. `-gcflags`, `-asmflags` and `-tags` are free-form by definition. This
is the one place where a string somebody typed into a CI pipeline can walk into
a payload.

**The key set is not ours.** The Go toolchain decides what it records and is
free to add keys in any release. Any rule expressed as "drop these" protects
only against the keys we thought to name at the time we wrote it, and silently
stops protecting the moment the toolchain grows a new one.

Separately, the payload introduced by ADR 0017 is named `go_dependencies` while
already carrying `go_version` and `main_module`. It was facts about a build from
the day it shipped, and settings make the name plainly wrong. No backend
consumes it yet, so the rename costs a golden file today and a protocol
migration later.

## Decision

**Build settings are filtered on the node by an exhaustive allow-list, and
everything else is discarded before a record is formed.** The list is
`CGO_ENABLED`, `GOARCH`, `GOAMD64`, `GOARM64`, `GOARM`, `-race`, `-trimpath`,
`vcs`, `vcs.revision`, `vcs.time`, `vcs.modified`. It lives in
`internal/nodescan`, which means the filtering happens before the node→controller
channel: nothing outside it crosses the wire, rather than crossing it and being
dropped (CLAUDE.md invariant 4).

Allow-list rather than deny-list is the decision, not an implementation detail.
Everything on the list is a token the toolchain chose from a fixed vocabulary or
a value git produced; a deny-list would have to keep pace with a key set we do
not control. The failure mode of an allow-list is a fact we do not collect. The
failure mode of a deny-list is a build hostname in a payload.

Three consequences follow directly:

**Only the PGO boolean survives, never the profile path.** `-pgo` is not on the
list, so its value — a directory on the build machine — stays on the node, while
`GoRecord.PGO` still says whether PGO was applied.

**`PGO` stays on the record even though the settings now imply it.** A record
whose container has no image digest yet has no `go_build` payload to join to;
that case is already counted as `facts_undigested` (ADR 0017). PGO is the one
build fact that survives a missing key.

**An over-long value is dropped whole, never truncated.** Every allowed key holds
something short and bounded, so a value beyond 128 characters means the binary is
not what the list assumes. A prefix of an unexpected string is still an
unexpected string, and would ship looking like a legitimate value.

**The payload is renamed `go_dependencies` → `go_build`**, with `settings`
alongside `modules`. The file in the spool becomes `go-build-<digest>.json`, and
the Go type `inventory.BuildDependencies` becomes `inventory.BuildFacts`. ADR
0018 refers to the old method names `PendingDependencies` /
`MarkDependenciesWritten`; those are now `PendingBuilds` / `MarkBuildWritten`.
ADR 0018 is not edited — an accepted ADR describes what was decided when it was
decided, and this paragraph is the forwarding address.

**`vcs.*` is collected, and its absence is normal.** A commit hash is an
identifier from the customer's repository. It carries no source, is meaningless
without access to that repository, and is enumerated in `docs/security.md` §8
like every other collected field. More importantly it is frequently missing: the
toolchain stamps it only when the build ran against a VCS working tree, and three
independent conditions have to hold in a container build — `.git` must reach the
build context (excluding it is a common and *correct* thing to do, since `.git`
changes on every commit and invalidates the `COPY` layer cache), the builder
image must have `git`, and the repository must be owned by the user running the
build. That last one fails loudly: `go build` with `-buildvcs=auto` silently
omits the stamp when there is no repository at all, but *errors the build* when
a repository exists and the VCS command fails, which is what
`COPY . .` followed by `USER nonroot` produces. The usual response is
`-buildvcs=false`, after which the stamp is gone for good.

Both halves of this were verified against real images from this repository: the
agent's own image carries `vcs.revision` and `vcs.modified=true`, while the e2e
sample image — built from `test/e2e/sample/`, a context with no `.git` — carries
no `vcs.*` at all.

**`node_metadata` gains `architecture`**, read from `status.nodeInfo.architecture`
of a node object the agent already watches. This is part of the same decision
rather than a separate one: `GOARCH` and `GOAMD64` are not findings on their own,
they are one half of a comparison, and without the node's architecture the other
half does not exist anywhere in the agent's data.

## Consequences

**Easier.** The build questions that need a settings block — race builds in
production, microarchitecture left on the table, architecture mismatches, dirty
builds — become answerable from the payloads, and the allow-list means the answer
never cost a build-machine path. `security.md` §8 can enumerate the collected
settings in full, in a table, because the list is finite by construction.

**Harder / given up.** Teams that stamp their revision through
`-ldflags -X main.version=…` instead of the toolchain's own VCS stamping lose it:
that flag is dropped whole. This is the correct trade — an allow-list that makes
an exception for one popular use of a free-form flag is not an allow-list — but
it is a real loss, and it is the common case in projects older than Go 1.18.

Adding a setting later means editing the list, `security.md` §8, and the
end-to-end test, which restates the list independently so that widening it in the
scanner alone cannot widen what leaves a real cluster. That friction is
deliberate: each addition is a change to what is collected, and CLAUDE.md already
requires those to be visible in the same PR.

**Not addressed here.** Whether the backend should treat a missing `vcs.*` as a
prompt to suggest enabling `-buildvcs` is an analysis question, outside the
agent. Build settings for non-Go workloads do not exist — this payload is
Go-only, as ADR 0009 established for the scanner as a whole.

This ADR records a decision implemented in the same pull request, per the
process in [`README.md`](README.md).
