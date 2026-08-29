# 0050. The GOMAXPROCS finding rests on a build fact, not on the toolchain version

Date: 2026-08-29

Status: Accepted

Amends: 0019, 0047

Adds two allow-listed GODEBUG defaults to `go_build`, parsed out of the compound
`DefaultGODEBUG` build setting. Corrects [ADR 0047](0047-runtime-knobs-are-named-and-kept.md)'s
Context, which states a runtime behaviour that stopped being universal in Go 1.25.

## Context

ADR 0047 justified keeping `GOMAXPROCS` and `GOMEMLIMIT` with a claim about the
Go runtime: "the runtime counts the node's cores, not the cgroup's quota". That
was true when it was written about every Go binary. It is now true about some of
them.

Go 1.25 made the runtime read the cgroup CPU quota when sizing `GOMAXPROCS`, and
made it follow a quota that changes while the process runs. Both are GODEBUG
settings with a changed default — `containermaxprocs` and `updatemaxprocs`,
`Old: "0"`, `Changed: 25` in the toolchain's own `internal/godebugs` table. So the
waste ADR 0047 calls the most common in Go on Kubernetes is, on a current
toolchain, already absent.

The version that decides this is not the one the agent collects. GODEBUG defaults
follow the `go` directive of the **main module**, not the toolchain that ran the
build: a binary compiled by Go 1.26 from a module declaring `go 1.21` gets the
pre-1.25 behaviour. `go_build.go_version` is the toolchain. Nothing the agent
ships distinguishes the two builds, so the finding fires on both — and the fleet
where it is wrong is the fleet that already did the work.

The toolchain records the difference. When a build's GODEBUG defaults deviate
from the toolchain's own, it writes them into a `DefaultGODEBUG` build setting,
which is exactly the fact the finding needs and is already sitting in the build
info the node scanner reads and discards.

## Decision

**1. `DefaultGODEBUG` is parsed on the node, and only named settings survive.**
The kept names are `containermaxprocs` and `updatemaxprocs`. They ship as a
`godebug` object on `go_build`, beside `settings`.

Parsing is the decision, not a convenience. ADR 0019 keeps build settings by an
allow-list of whole values, and `DefaultGODEBUG` is a comma-joined list of every
deviating setting — six of them for a module on `go 1.21`, most of them about
TLS, `net/http` and `go/types`. Allow-listing it whole would ship all of those,
and would then drop the value entirely against ADR 0019's 128-character bound —
which lands on long lists, which are the old `go` directives, which are the only
builds this field exists to find. The failure mode would be silence on exactly
the population the finding is about.

The two names satisfy ADR 0019's own test for the list: a token the toolchain
chose from a fixed vocabulary, holding `0` or `1`. Nothing operator-written
enters through them. The other sixty names in the table decide nothing about
resources and are not collected.

**2. An absent name is not a value, and the backend is told so.** The toolchain
records only what deviates from its own defaults, so `containermaxprocs` absent
means either "this build gets the current default" or "this toolchain predates
the setting". `go_build.go_version` separates them, and the two fields are read
together or not at all — the discipline of [ADR 0048](0048-findings-name-the-fields-they-need.md) §2,
applied to a field whose absence is the common case.

There is a third input, already collected: `GODEBUG` set in the container's
environment overrides the compiled default at startup, and ADR 0047 keeps that
variable in `workload_metadata.runtime_env`. Build fact and runtime override are
different claims about different objects; the agent ships both and joins
neither.

**3. ADR 0047's Context is corrected here rather than edited there.** An accepted
ADR's body is immutable ([`README.md`](README.md)), so this paragraph is the
forwarding address: ADR 0047 §1's decision — five names, closed list, applied at
the informer transform — stands unchanged. What does not stand is the sentence in
its Context that describes the pre-1.25 runtime as the runtime.

## Consequences

**Easier.** The finding names the builds where it is true. A cluster whose
services are all on a current `go` directive gets silence, which is the answer,
and the report can say what it checked rather than listing every Go service.
`updatemaxprocs` adds a finding that did not exist: a workload whose CPU limit
was resized in place, whose runtime never noticed.

The `/proc/<pid>/status` read that was to supply `Cpus_allowed_list` for this
same finding is not needed for it. That read may still be worth making for
`VmHWM`, on its own argument, and would arrive with its own ADR and its own
extension of ADR 0009's node scope.

**Harder / given up.** A second allow-list, in a second shape, in the same
function's neighbourhood. `buildSettingsAllowList` keeps whole values by key;
this one keeps parsed pairs out of one value. Two mechanisms doing similar work
is a cost, accepted because collapsing them would mean either shipping the
compound string or teaching the first list about structure inside a value.

The list will need revisiting when the runtime grows another knob that reads the
cgroup. That is a maintenance obligation on a file, not on a payload: adding a
name changes what is collected and needs `security.md` in the same PR, which is
the mechanism working.

**Not changed.** No new RBAC, no new read, no new privilege: `DefaultGODEBUG` is
in build info the node scanner already opens, and the parse happens before the
node→controller channel, so nothing outside the list crosses the wire (CLAUDE.md
invariant 4). `go_build` remains write-once and keyed by image digest; the new
object is another immutable property of the build (ADR 0017).

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
