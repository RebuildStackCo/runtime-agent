# 0086. A toolchain move is a review of what the toolchain reports

Date: 2026-09-17

Status: Accepted

Amends: 0081 §1

Moves the toolchain floor from Go 1.26.7 to 1.27.1 in `go.mod` and in the build
image together, adds `fips140ems` to the GODEBUG allow-list, and keeps four
names Go 1.27 removed. No payload shape changes, no RBAC, no chart change.

## Context

ADR 0085 gave Dependabot the Docker ecosystem, and its first pull request
proposed Go 1.26.7 → 1.27.1 — a minor release, not a patch. The guard held: one
check failed, `go.mod` and the build image disagreed, and ADR 0038 §3 had already
said why that must not merge. CI's `setup-go` reads `go-version-file: go.mod`, so
the vulnerability gate would have been proving something about a standard library
the released image does not contain.

Moving the floor by hand is one line in two files. What makes it a slice is what
sits under those lines: this agent reports GODEBUG defaults out of other
people's binaries (ADR 0050, ADR 0081), and a minor release is exactly when that
vocabulary changes. Measured on go1.27.1 against go1.26.7, it changed in both
directions.

**Four names are gone.** Go 1.27 removed `tls10server`, `tls3des`, `tlsrsakex`
and `tlsunsafeekm` — the toolchain's `internal/godebugs` now carries a `Removed`
list, and using an old value is a build error rather than a weakened build. Four
of the fourteen names ADR 0081 chose are among them.

**One name is new, and it is the kind this list is for.** `fips140ems`, added in
1.27 and backported to 1.26.6, has no version-dependent default: it reaches a
binary only because someone wrote a `//go:debug` line. What that line turns off
is stated in the toolchain's own `doc/godebug.md` — the enforcement of extended
master secret in FIPS 140-3 mode.

## Decision

**1. `fips140ems` goes on the allow-list.** It passes both of ADR 0081's tests.
Its one-line justification is written from the standard library's own
documentation, which is §4's bar. And it is a name only a directive can set,
which §1 called the stronger signal: a build that lags says someone did not
upgrade, a build carrying this says someone decided.

Measured rather than assumed, as ADR 0081's names were: a `go 1.27` module
carrying `//go:debug fips140ems=0` emits `DefaultGODEBUG=execerrdot=1,fips140ems=0`
under go1.27.1.

Two other names arrived with `Changed: 27` and neither goes on the list.
`tracebacklabels` governs what a traceback prints. `x509sslcertoverrideplatform`
decides whether `SSL_CERT_FILE` and `SSL_CERT_DIR` override the platform root
store — but only on Windows and Darwin, and its *old* value is the stricter one,
so its presence in a Linux container's build would mean nothing this agent could
report honestly.

**2. The four removed names stay.** They are removed from the toolchain, not
from the world. This list describes binaries in a customer's cluster, and those
were built by every toolchain rather than by ours: an image built last year with
Go 1.25 still carries `tls10server=1`, and that is still the fact worth
reporting. Deleting them would blind the agent to exactly the old builds the
list exists to find.

What changes is only what a *new* build can say: a binary built by Go 1.27 is
silent about all four, and silence there now means the knob does not exist
rather than that the default was kept. Absence has always meant "the toolchain's
own default" (ADR 0050 §2), and this does not change that reading — a default
that can no longer be overridden is still the default.

**3. The measurements are fixtures, and the old one stays.** The `go 1.21`
default set measured under go1.26.7 remains a test input: it is what a binary in
a cluster looks like, and most of them were not built this week. The set measured
under go1.27.1 is added beside it, which is what pins the two claims above — that
the four removed names cannot appear in a new build, and that every name that
does appear is still one the allow-list knows.

**4. Two tools move with the floor, and one of them had to.** `golangci-lint`
v2.11.4 is built with Go 1.26 and refuses a module targeting 1.27 before reading
its configuration — not a finding, a hard stop. The pin moves to v2.13.2, whose
published binary is built with go1.27.0.

That version has opinions the old one did not, and one of them is worth the
slice on its own: `ecdsa.PublicKey`'s `X` and `Y` have been deprecated since Go
1.26, and `internal/nodeauth` built every JWKS key by assigning them. It now
assembles the uncompressed point and calls `ecdsa.ParseUncompressedPublicKey`,
which rejects a point that is not on the curve. The comment that stood there —
that an off-curve point cannot verify a signature anyway, so no check was needed
— was true and is no longer the argument: a JWKS entry that cannot be a key is
refused where it is read, not where it is used.

The second finding the new version reports is a false one, and only off Linux:
on a platform where `startCapture` is a stub that always fails, `err != nil` is
always true. CI lints the Linux build, where it is not.

## Consequences

**Easier.** The floor is current, so the stdlib in the image is the stdlib
`govulncheck` inspects. The agent reports one more deliberate weakening, in the
class where a finding names a decision rather than a lag.

**Harder / given up.** The allow-list now outlives the toolchain that defined
it, and it will keep growing that way: names are added when a release adds them
and never removed when a release removes them. That is the right asymmetry for a
list about other people's binaries, and it means the list's length is not a
measure of anything.

A minor Go release changes more than this ADR reviews — language, runtime,
library behaviour. What is claimed here is only that the facts this agent
*reports* were re-measured against the new toolchain, and that the test suite,
the vulnerability gate, the chart and the end-to-end run pass on it.

**Not changed.** The payload's shape, the reading the backend is asked to make
of an absent name, and every other decision in ADR 0050 and ADR 0081. The
`ecdsa` change is a rewrite of how one key is constructed, not of which keys are
accepted — except the off-curve ones, which never verified anything.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
