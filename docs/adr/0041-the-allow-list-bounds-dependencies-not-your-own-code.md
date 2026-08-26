# 0041. The symbol allow-list bounds dependencies, not the customer's own code; source paths do not ship

Date: 2026-08-26

Status: Accepted

Amends: 0011 §4, 0039 §5

Cuts the compiler-recorded source path down to a base name before it enters the
agent, and corrects what the symbol allow-list was ever meant to bound. No
change to RBAC, to the chart's privileges, or to which workloads are profiled.

## Context

ADR 0039 recorded two ways a frame gets past the symbol allow-list and deferred
closing both to a slice of its own. Building that slice showed the two are not
the same kind of thing, and only one of them is a defect.

**The exemption for the customer's own code is not a defect.** `SymbolFilter.keep`
returns early for any package whose path has no dot in its first segment, which
covers the standard library, the workload's own `main`, and any module declared
without a domain (`module acmecorp`). ADR 0011 §4 and `docs/security.md` §8 both
describe the policy as "kept only if the module path is on the allow-list", so
the implementation looked like it had drifted.

Closing that gap means requiring a customer to enumerate their own module paths
before they can see their own hot functions. Two things are wrong with it.

The first is that ADR 0025 already removed exactly this shape. It abolished the
profiling eligible set on the grounds that profiling scope *is* collection
scope, and that a second list to keep in step with the first is a trap rather
than a control. A customer excludes workloads with namespace filters and opt-out
annotations (ADR 0015, ADR 0028); that is the mechanism for "what do you
collect". Gating the customer's own `main` behind a second list reintroduces the
trap in the one place it is most visible: install the agent, allow the
namespace, and half the stack reads `[filtered]`.

The second is that the heuristic is sounder than its critics. A public Go module
path must begin with a domain or it cannot be fetched, so "no domain in the
first segment" means, in practice, *the standard library or code the customer
did not publish* — and both are what they installed the profiler to see.

Nor does keeping it widen anything. Profiling targets come from the controller's
admitted-pod index, so a controller — buggy or hostile — can only aim at
workloads the customer's own filters already admitted. Always keeping `main`
therefore exposes nothing beyond the function names of admitted workloads, which
is the product.

**The source path is a defect, and a plain one.** `traceReporter` copied
`f.SourceFile` verbatim, `keep` never looked at the field, and `Serialize` wrote
it into the shipped pprof as `Function.Filename`. A Go binary built without
`-trimpath` — the default — records the absolute path from the machine that
compiled it, so a shipped profile carried strings like

    /home/jenkins/workspace/acme-payments/src/git.acme-internal.example/payments/api/server.go

That is the CI workspace, an internal VCS hostname, the customer's project name
and the layout of their source tree. It is not their code structure, which is
what a profile is for and what they agreed to ship. It is our sampling carrying
their build machine out of the cluster — the same class of string ADR 0019
excludes `-ldflags`, `-gcflags` and `-tags` from the scanner's build-settings
allow-list to avoid, one package away.

## Decision

**1. The allow-list bounds third-party and infrastructure frames.** It is not,
and was never usefully, a gate on the customer's own code. ADR 0011 §4's "kept
only if its Go module path is on the client allow-list" is corrected to: frames
of the customer's own code are kept — the standard library, the workload's
`main`, and modules declared without a domain — while frames of domain-bearing
modules that are not on the allow-list are redacted, third-party by default.

The load-bearing claim of ADR 0025 §5 survives unchanged, and is worth restating
because it is easy to read this as a weakening: the allow-list is still what
stops a hostile controller from extracting the structure of anything beyond the
customer's own admitted workloads. What changes is only the description of which
frames it was ever bounding.

**2. The source file is reduced to its base name, at the seam.** `server.go`,
never a directory. Cut in `traceReporter.ReportTraceEvent`, where the profiler
hands the frame over, so the full path never enters the buffer and there is no
stage at which the agent holds it — the filter-early rule of CLAUDE.md
invariant 4, applied to the one field that had escaped it.

Both separators are cut, not the host's: the path was recorded by whatever
machine compiled the binary, which need not be the one running it.

**3. The line number stays.** It is a bare integer that says nothing about the
machine that compiled the binary, and with the base name it is what a flame
graph shows. Dropping it would cost the customer something and buy nothing.

**4. The property is asserted on the shipped bytes, not only at the seam.** A
test parses a serialized profile and requires that no `Function.Filename`
contains a path separator. The reporter is what cuts the path and the serializer
is a package away; an assertion only at the seam would miss a second producer of
frames, and this one would still catch it.

**5. `docs/security.md` §7.2 and §8 say both of these.** They currently name
both as open gaps, per ADR 0039. One is closed and the other was never a gap,
and the document should not carry a warning about a design decision as though it
were an outstanding defect.

**6. The chart still refuses an empty allow-list.** It is tempting to conclude
that an empty list is now harmless, since the customer's own code is kept
regardless. It is not: a customer whose modules are `github.com/acme/…` would
get `main` and the standard library and lose their entire service layer, which
is a worse profile with no warning. The refusal stays; only its wording is
corrected, since it currently claims the profiler would "ship nothing".

## Consequences

**Easier.** A customer installs the agent, allows a namespace, and sees their
own functions — with no second list to maintain and no support ticket asking
why half the stack is redacted. The allow-list now does one job and the document
describes that job.

**Harder / given up.** Less than it first appears, and the reason is worth
recording because "three of my files are called `server.go`" is the obvious
objection.

For Go frames nothing is lost. A symbolized Go function name carries its full
package path, and a Go package is a directory, so two files of the same name
cannot share one. `github.com/acme/app/api.(*Server).Serve` plus `server.go`
identifies the file exactly, and three `server.go` in three packages stay three
distinct `Function` entries in the shipped profile — measured, not assumed. What
the base name removes is only the prefix *before* the package path, which is
precisely the build machine's part of it.

What is genuinely lost is for frames whose name carries no package path — bare
native symbols from libc or the kernel. There the base name is the only file
hint and can now collide with an unrelated one. Those frames are not the
customer's code: unsymbolized frames are redacted anyway, and kernel frames are
kept for readability rather than for their filenames.

Anyone who wanted to map a profile back onto a source tree by absolute path
loses that. It was never something to offer — it worked only because the
customer's build machine was leaking into their telemetry.

The base name is a reduction, not a redaction: a filename is still a string the
customer wrote, and `payroll_export.go` says something. That is the same
category as the function names beside it, which is the thing the profiler exists
to ship, so no further reduction is proposed.

**Not changed.** No RBAC, no privilege, no chart template beyond one refusal
message, and no change to which workloads are profiled or which frames are
redacted. Only the path is shorter.

This ADR records decisions implemented in the same pull request, per the process
in [`README.md`](README.md).
