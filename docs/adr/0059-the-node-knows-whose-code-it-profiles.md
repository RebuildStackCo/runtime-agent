# 0059. The node knows whose code it profiles, so the allow-list stops being a prerequisite

Date: 2026-08-30

Status: Accepted

Amends: 0011 §4, 0041

Closes, for the eBPF capture path, the defect
[ADR 0058](0058-the-pull-starts-a-profiler-and-the-binary-says-whose-code-it-is.md)
named as open. Removes the chart's refusal to install `ebpf` without a
configured allow-list. No new read, no new privilege, no change to what may
leave.

## Context

The symbol filter classifies a frame by module path: no domain means standard
library or unpublished code and is kept, an allow-listed prefix is kept, and
anything else is third-party and redacted. A customer's own service lives under
`github.com/acme/web`, which has a domain. So the configured list was not an
addition to what the filter knew — it was the only thing standing between the
customer's service layer and the placeholder.

ADR 0058 fixed this for pulled profiles by admitting the modules a build states
it was compiled from. The capture path was left as it was, with two mitigations
that look like more than they are. The chart refuses to install `ebpf` with an
empty list, which forces one prefix and says nothing about the second service.
And `nodeprofile.Validate` rejects a profile with nothing but runtime and
placeholder frames in its top, which turns a redacted service into silence
rather than into a wrong answer.

The information needed was already on the node and already in the same process.
`cmd/agent/node.go` runs the scanner, which reads every kept binary's main
module and every dependency a `replace` redirected, per container, every pass.
`cmd/agent/pipeline.go` ran the profiler beside it and built one filter for the
whole node, from configuration alone. The two goroutines never spoke.

This was not visible from the tests because the capture end-to-end test
configures the sample's own module path, which is exactly the configuration a
customer would have to discover.

## Decision

**1. The scanner publishes what it read; the profiler reads it per window.** A
map from container to the module paths its build compiles from source, replaced
wholesale on each pass, so a container no live process belongs to leaves with
it. The mechanism is the one `internal/targeting` already uses on the
controller: an atomic pointer to an immutable map, because a pass writes while a
window reads.

The two sets align by construction and this is worth stating rather than
assuming: the scanner walks only pods the scope admits, and the profiler ships
only containers in targets ∩ scope, so a profiled container is a scanned one.

**2. The filter is built per container, not per node.** `processWindow` already
grouped samples by container; the allow-list for each group is the configured
prefixes plus that build's own modules.

A container the scanner has not reported yet — the first window after start can
outrun the first pass — falls back to the configured list, which is exactly the
previous behaviour, and `Validate` still refuses a profile of nothing but the
runtime. No new drop rule was invented for a case the existing one covers.

**3. An entry that cannot be a module path is refused.** Part of the allow-list
now comes from binaries rather than from a file, so a build declaring its main
module as a bare `github.com` would otherwise admit every module hosted there.
An entry must have a domain-bearing first segment and something after it.

The same check applies to a prefix an operator wrote, where the same mistake is
available and was previously accepted. That is a behaviour change for a
misconfigured install and it is stated here rather than left to be discovered.

This is not a defence against a hostile workload. A binary that lies about its
module exposes its own dependency frames to the party the customer already
trusts with its profiles. It is a defence against a plausible typo having an
implausible blast radius.

**4. The chart stops requiring what nobody needs.** The refusal to render `ebpf`
with an empty allow-list existed because an empty list produced nothing, forever,
silently. That stopped being true with §1. A required value that changes nothing
is the inert knob ADR 0025 abolished, so it goes, and `ebpf` installs and
produces useful profiles with no profiling configuration at all.

**5. ADR 0011 §4's reasoning is preserved, not weakened.** That decision put the
allow-list in a file Helm owns so that "no reply from the controller can widen
it". The modules added here are read by the node from the binary it is already
profiling; the controller is not in the path. Both halves of the list are
therefore node-held, and the table in `security.md` §7.2 that separates bounds
the node holds from facts it receives keeps this row on the right side.

## Consequences

**Easier.** `ebpf` becomes installable with nothing configured and produces a
profile of the customer's own code, which is what `inventory` plus pulling has
done since ADR 0058. The two capture paths now answer the same question the same
way, and the configured list means what its name says: extra prefixes, for a
shared internal module a build requires rather than replaces.

One derivation of "the modules a build compiles from source" now serves both
paths, in `nodescan`, rather than one copy per consumer.

**Harder / given up.** A misconfigured allow-list entry is now dropped rather
than used. An operator who wrote `github.com` and believed it worked will find
that it does not — which was always true of the frames it kept, and is now true
of the entry itself.

The first capture window after a node starts can precede the first scan pass, so
one window of one container may be filtered against configuration alone. It
costs a window and corrects itself; a capture is not a counter and nothing
accumulates.

The eBPF capture path still contributes nothing to `collection_coverage`. The
prober and puller each report what they attempted and why it failed; the
profiler's own refusals live in the node's log. So "this workload has no eBPF
profile" is still indistinguishable from a broken agent, which is precisely what
ADR 0054 exists to prevent, and it remains open.

**Not changed.** No new file is read, no new privilege, no change to which
frames may leave — the modules admitted here are the customer's own code, which
the allow-list existed to keep. The node still holds no Kubernetes identity.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
