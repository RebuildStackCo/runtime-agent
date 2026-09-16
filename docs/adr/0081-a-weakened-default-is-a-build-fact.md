# 0081. A weakened security default is a build fact like any other

Date: 2026-09-16

Status: Accepted

Amends: 0050 §1

Adds twelve allow-listed GODEBUG names to `go_build`, on a second axis from the
two [ADR 0050](0050-godebug-defaults-are-a-build-fact.md) opened the parse for.

## Context

ADR 0050 opened `DefaultGODEBUG`, kept `containermaxprocs` and `updatemaxprocs`,
and closed the door behind them in one sentence: "The other sixty names in the
table decide nothing about resources and are not collected." That was correct on
the axis it was written on. It is now the thing in the way, because the axis was
never the only one.

What changed is the question, not the toolchain. The agent already ships each
build's module list, its toolchain version and its VCS stamp — enough for a
reader outside the cluster to ask what a binary is exposed to. GODEBUG defaults
are the one input to that question the node scanner opens and discards.

The measurement, on go1.26.7. A main module declaring `go 1.21` gets a
`DefaultGODEBUG` of 30 names in 502 characters. Nine of them hold a security
default at its pre-tightening value:

| Name | At this value | What it restores |
|---|---|---|
| `tls10server=1` | 1 | TLS 1.0 accepted by servers |
| `tls3des=1` | 1 | 3DES cipher suites offered |
| `tlsrsakex=1` | 1 | RSA key exchange, so no forward secrecy |
| `tlssha1=1` | 1 | SHA-1 handshake signatures accepted |
| `tlsunsafeekm=1` | 1 | keying material exported without extended master secret |
| `x509negativeserial=1` | 1 | certificates with negative serial numbers parsed |
| `httplaxcontentlength=1` | 1 | invalid `Content-Length` headers accepted |
| `cryptocustomrand=1` | 1 | `crypto/rand.Reader` replaceable by the program |
| `rsa1024min=0` | 0 | RSA keys under 1024 bits accepted |

Nobody chose those nine. They follow from the main module's `go` directive, the
same directive ADR 0050 §2 already identified as the fact that decides GODEBUG
defaults. Nothing in the image, the manifest or the container environment states
them, and a scanner that reads the image cannot find them: the value exists only
inside the binary, in a build setting the toolchain wrote.

Three names run the other way. `execerrdot`, `tarinsecurepath` and
`zipinsecurepath` have no version-dependent default, so no `go` directive emits
them; they reach a build only through an explicit `//go:debug` line. Measured: a
`go 1.26` module carrying three such lines emits exactly those three names and
nothing else. A build that lags is one fact; a build where someone wrote the
directive is a different and sharper one, and the same field carries both.

## Decision

**1. Twelve names join the allow-list, on one test.** A name is kept when it is
not `Opaque` in the toolchain's `internal/godebugs` table, holds `0` or `1`, and
one of its two values is a documented weakening of a security property the
toolchain tightened. The nine above, plus the three a directive alone can set.

This is ADR 0019's test for the list unchanged — a bounded token the toolchain
chose from a fixed vocabulary, nothing operator-written entering through it —
applied to a second kind of question. The parse ADR 0050 §1 built is not touched:
only the set of names it keeps.

**2. The agent ships the pair and names no finding.** Which of the two values is
the weak one differs by name — `tls10server=1` and `rsa1024min=0` both weaken —
and the collector does not encode that. It is a judgement, judgement happens
outside the cluster, and a mapping compiled into the agent would be a second
place for the toolchain's table to be wrong.

**3. ADR 0050 §2's reading obligation extends unchanged, and splits.** An absent
name is still not a value. For the nine version-dependent names, absence means
the build takes its toolchain's own default, and `go_build.go_version` separates
that from a toolchain predating the setting. For the three directive-only names,
absence means the directive was not written — there is no version that supplies
them, so `go_version` says nothing about them at all.

**4. What is refused, and on what rule.** `Opaque` names — `tlsmlkem`,
`tlssecpmlkem`, `dataindependenttiming`, `fips140` — are marked by the toolchain
as not reducible to a reported on/off, so keeping them would ship a value whose
meaning we would be guessing. Names whose value is a number rather than `0` or
`1` (`httpcookiemaxnum`, `urlmaxqueryparams`, `tlsmaxrsasize`) fail the bounded-
token test. `urlstrictcolons` fails a third rule this ADR states for the first
time: **a name whose one-line justification cannot be written from the standard
library's own documentation does not go on a list whose entries are justified one
line each.** Its meaning appears nowhere outside the metrics table.

## Consequences

**Easier.** A fleet-wide question that was unanswerable from any payload becomes
a lookup in one that already ships. A build on an old `go` directive is legible
as what it is, per digest, for every running binary — and the honest phrasing of
the finding follows from the Context: nobody typed `tls10server=1`.

The two directive-only names about archive paths and the one about `PATH`
lookups arrive as a class the agent had no way to see: a deliberate opt-out of a
hardening, recorded in the binary by the person who wrote it.

**Harder / given up.** The list now has two axes, and ADR 0050's maintenance
obligation doubles with it: every Go release that tightens a default is a
candidate name, and adding one changes what is collected and moves
`docs/security.md` in the same PR. That is the mechanism working, but it is now
worked twice as often.

The sixty-odd names still outside remain outside. A build that weakens something
this list does not name is silent, and the silence is indistinguishable from a
build that weakens nothing — the same shape of gap ADR 0054 exists to report at
the level of counts, and which is not reported at the level of names here.

**Measured cost.** The golden `go_build` payload grew from 836 to 959 bytes for
five added names. `go_build` is write-once per image digest (ADR 0017), so this
is paid once per image and never per flush; a build that deviates in nothing
still ships no `godebug` object at all.

**Not changed.** No new RBAC, no new read, no new privilege. `DefaultGODEBUG` is
in build info the node scanner already opens, and the parse runs on the node
before the node→controller channel, so nothing outside the list crosses the wire
(CLAUDE.md invariant 4). This does not make the agent a security scanner: it
ships one more immutable property of a build, and reads nothing to get it.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
