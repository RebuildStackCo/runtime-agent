# 0085. The image is built and scanned where the code is, and pinned to what it was built from

Date: 2026-09-17

Status: Accepted

Amends: 0038 §1

Adds an image build and a base-layer scan to CI, pins both `FROM` lines by
digest, gives the build a context that excludes private notes, and lets
Dependabot watch the two pins. No payload byte, no RBAC, no chart change, and
nothing is published anywhere.

## Context

The DaemonSet runs as root, with `hostPID` and `CAP_SYS_PTRACE`, and with
`CAP_BPF` and `CAP_PERFMON` in one profile. `security.md` §7.1 states the
consequence plainly: installing it "is a decision to trust this agent's image
with the node". Everything in this repository bounds what the agent's code does;
none of it bounds what *other* code in that container could do.

So the image deserves at least the discipline the code already has, and it had
none.

**Nothing built it but a person.** `make image` ran on a laptop, before an e2e
run. A Dockerfile that does not build reached `main` and waited there for
whoever next needed a cluster.

**Nothing looked inside it.** `make vulncheck` answers about Go, on reachability
(ADR 0038), and is the stronger tool for that question. It cannot see the base
layer at all. What is in that layer was never enumerated: six Debian packages,
listed with their versions in `/var/lib/dpkg/status.d`, which no tool here read.

**Both bases were tags.** `golang:1.26.7` and `gcr.io/distroless/static:nonroot`
are names their owners may move. A rebuild of a commit was not a rebuild of the
same bytes, and a moved tag would have arrived silently.

**The build context was everything.** With no `.dockerignore`, `COPY . .` took
`CONTEXT.md` — the file this repository's own rules say must never reach a
committed artifact — into the builder stage. It does not reach the published
layers, and that is luck about which stage gets exported rather than a decision.

## Decision

**1. CI builds the image on every pull request, and pushes nothing.** A build is
a check like `go build`; publishing is a release, which is a separate decision
with separate questions (signing, provenance, multi-architecture) and is not
made here.

**2. The base layer is gated; Go is not, and that division is the point.**

The image is catalogued with `syft` and the catalogue is scanned with `grype`,
which fails the build on a high-or-worse finding that has a fix. `.grype.yaml`
ignores every `go-module` finding, and grype itself says why in a warning it
prints on our image: without function symbols, "go vulnerability matching falls
back to module granularity and may report false positives".

That is precisely the question ADR 0038 answered better. Its §2 decided a
finding reported but not called must not fail the build, on a module that has no
fixed version and is not linked into the agent at all. A scanner matching
versions against the binary's embedded module list would fail on exactly that
class. So Go findings are printed — `--show-suppressed`, so the choice stays
visible — and the base layer, which govulncheck cannot see, is what gates.

**3. The catalogue is asserted before it is trusted.** A cataloguer that finds
nothing reports nothing, which is indistinguishable from a clean image. The job
fails unless the base layer yields at least its six packages and the binary at
least sixty modules.

**4. Both bases are pinned by digest, and the pins are watched.** A digest makes
a rebuild reproducible and a moved tag impossible. It also makes the pin a thing
someone must update, which is how pins rot — so Dependabot gains a third
ecosystem beside the Go modules and the actions it already watched, and an
updated base arrives as a pull request this same job builds and scans.

That does not make Dependabot the vulnerability gate, and ADR 0038 §1 stands:
what it can answer about Go is one transitive module absent from the shipped
image, while sixteen reachable standard-library vulnerabilities went unseen.
Watching a pin and gating a build are different jobs.

One consequence needs a guard. Dependabot can now move the builder's tag on its
own, which would break the rule ADR 0038 §3 stated — that the floor in `go.mod`
and the floor in the build image are one fact. `test/image` fails when they
disagree.

**5. The context excludes private notes and keeps `.git`.** `CONTEXT.md` leaves
the context. `.git` stays, deliberately: it is what stamps `vcs.revision`,
`vcs.time` and `vcs.modified` into the binary, and that build info is exactly
what this agent reads off customer workloads to answer "which commit is
running". An image of ours that could not answer it would fail the check we sell.

**6. `-trimpath`.** The binary carried `/src/...` from the builder. One fewer
difference between our build of a commit and anyone else's.

## Consequences

**Easier.** A broken Dockerfile fails a pull request. What is inside the image is
enumerated rather than assumed — six packages and their versions, the binary's
module list — and a customer asking what the container is made of has an answer
that came from the artifact rather than from a claim. The pins make a rebuild
mean something.

**Harder / given up.** Two more container images in CI's dependency set, both
pinned, and a scan that needs a vulnerability database over the network — the
same non-hermetic cost ADR 0038 already accepted. Base image updates now arrive
as pull requests someone must merge; a pin nobody updates is worse than a tag,
and this is the maintenance that keeps it from becoming one.

The gate is `--only-fixed`. A high-severity finding with no available fix will
be printed and will not fail the build, because failing on it offers no action
except pinning to an older base. If one appears, the decision is to rebase or to
state it, and both belong in a pull request rather than in a red CI run nobody
can clear.

**Not changed.** Nothing is published, signed or attested; the chart still names
its image by tag and cannot pin a digest; the build is still single-architecture.
Those are the release questions, and they are not answered here.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
