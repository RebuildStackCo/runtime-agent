# 0002. Wire format: protobuf over plain HTTPS POST

Date: 2026-07-28
Status: Accepted

## Context

The agent's traffic is batch upload — hourly rollups, profiles, metadata —
not interactive streaming. The environments it ships from are enterprise
networks: egress allowlists, TLS-inspecting proxies, HTTP/1.1-only middleboxes.
Bare gRPC depends on HTTP/2 trailers, which is exactly the feature such
infrastructure breaks most often.

We still want a typed, versioned, compact payload schema with well-understood
evolution rules.

## Decision

- Payloads are defined in protobuf; the `.proto` files are public and are the
  contract between agent and backend.
- The data plane is protobuf messages over plain HTTPS POST. Connect-style
  RPC framing is acceptable; bare gRPC is not part of the contract.
- Everything must work over HTTP/1.1 through a corporate proxy.
- Schema evolution follows protobuf field rules; the backend accepts payloads
  from at least the last two minor agent versions (N-2 window).
- The operator-facing control plane (cluster registry, tokens) is REST/JSON
  with an OpenAPI definition — its consumers are humans and automation, not
  the agent.

## Consequences

- One less failure mode in enterprise networks; "does it work through your
  proxy" is answered by "it is plain HTTPS."
- Schema compatibility is mechanically enforceable: a breaking `.proto`
  change fails CI (`buf breaking`).
- No streaming semantics; anything resembling streaming must be redesigned
  as batched upload, which matches the spool architecture (ADR 0003).
