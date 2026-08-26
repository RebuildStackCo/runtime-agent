# 0038. Vulnerabilities are gated on reachability, not on the module graph

Date: 2026-08-27

Status: Accepted

Adds `govulncheck` as a CI gate, moves the Go toolchain floor to 1.26.7, and
bumps one transitive module past an advisory. No change to what is collected, to
any payload, to the chart, or to any RBAC rule.

## Context

A push raised a Dependabot alert: `oras.land/oras-go/v2` v2.6.1, a high-severity
tar-extraction escape (CVE-2026-50163), fixed in v2.6.2. It arrives through
`helm.sh/helm/v4`, which this repository added as a tool dependency when the
chart became the installer (ADR 0036).

Two measurements said the alert was, for us, noise:

- `govulncheck` places it in "modules you require, but your code doesn't appear
  to call". The vulnerable path is tar extraction from an OCI registry; the only
  helm entry points this repository calls are `loader.LoadDir` and
  `engine.Render`, over a local directory.
- `go list -deps ./cmd/agent` links **zero** helm or oras packages. The code is
  not in the shipped image at all — it exists only in the test and tooling path.

The same scan, run because the alert prompted a look rather than because
anything asked for it, reported something else:

> Your code is affected by **16 vulnerabilities from the Go standard library**.

The toolchain was pinned at Go 1.26.1 in `go.mod` and in the build image, with
fixes spread across 1.26.2 through 1.26.6. These were not module-graph entries
nobody reaches. Their traces start in shipped code and cluster on the trust
boundary the security document leans on: four through
`internal/nodeintake/server.go` into `x509.Certificate.Verify`, three into
`tls.Conn.HandshakeContext`, five through `internal/nodeauth/jwks.go`, which is
what validates a node's projected token against the cluster JWKS. Among them a
`crypto/x509` authentication bypass through case-sensitive `excludedSubtrees`
name constraints, and an unauthenticated TLS 1.3 `KeyUpdate`.

So the alerting that exists found one transitive dependency absent from the
shipped image, and could not see sixteen reachable vulnerabilities inside it.
That is not a failure of Dependabot; it answers a different question. It walks
the module graph, and the standard library is not in it.

## Decision

**1. The gate is reachability.** `make vulncheck` runs `govulncheck ./...` in
CI. It fails only on vulnerabilities the code actually calls, and it covers the
standard library, which is where all sixteen were.

**2. A finding that is reported but not called does not fail the build**, and
that is the correct line rather than a convenience. `golang.org/x/crypto/openpgp`
arrives through helm, is declared unmaintained with no fixed version — so a gate
demanding zero findings would fail forever — is not called by this code, and is
not linked into the agent binary. Reporting it and not failing on it states
exactly what is true.

**3. The toolchain floor moves to 1.26.7**, in `go.mod` and in the build image
together. Both are the same fact and drifting them apart would mean CI proving
something about a binary the release does not build.

**4. `oras.land/oras-go/v2` moves to v2.6.2 directly**, rather than waiting for
a helm release that requires it. Minimal version selection takes the maximum, so
one line in `go.mod` satisfies helm's own requirement and clears the alert. This
does not fix a risk — there was none to fix — it removes a standing report that
would otherwise train its readers to ignore reports.

## Consequences

**Easier.** A vulnerability reachable from shipped code now fails a pull request
instead of waiting for someone to think of scanning. The scanner is a pinned
tool dependency alongside `kind` and `helm`, so nothing needs installing and the
scanner's own version cannot drift between machines.

**Harder / given up.** The toolchain floor is now something to maintain: Go
patch releases will make this gate fail, and each will need a bump rather than a
suppression. That is the intended cost — the alternative is the state this ADR
was written from, where the floor was six patch releases behind and nothing
said so.

The gate needs the vulnerability database over the network, so a CI run cannot
be fully hermetic.

**Not changed.** Nothing about collection, payloads, filtering, the chart or
RBAC. The bumps are a toolchain and one indirect module; no agent behaviour
depends on either.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
