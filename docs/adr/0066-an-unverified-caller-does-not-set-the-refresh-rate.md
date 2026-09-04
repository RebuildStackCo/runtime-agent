# 0066. An unverified caller does not set the refresh rate, and a value outside the enumeration does not load

Date: 2026-09-03

Status: Accepted

Amends: 0010

Two places where input the agent does not trust decided something quietly. A
JWKS refresh is now rate-limited, because the key ID that triggers it comes out
of a token nobody has verified yet; and a `thirdPartySymbols` outside its two
values is a startup failure rather than a silent fall back to `drop`.

## Context

**The refresh.** `CachingKeySource` refetches the cluster JWKS whenever it is
asked for a key ID it does not hold — the signal that the cluster rotated its
signing keys (ADR 0010). Rotation is rare, so the refetch looked rare.

It is not, because of where in the request the key ID is read. `jwt.ParseWithClaims`
calls the key function *before* it checks the signature, and the handler calls
`Verify` *before* it reads the body. So a caller that can reach the intake port
supplies the `kid` at no cost to itself, and each distinct value it invents cost
two GETs to the API server — discovery and JWKS — serialized behind this type's
one mutex with a ten-second ceiling each. The refresh rate was a property of the
caller.

Reaching the port is not free: the chart's NetworkPolicy admits only the node
DaemonSet, and the cluster's CNI has to enforce it (ADR 0039). That bounds who
can do this; it does not make the controller's traffic to its own API server
someone else's variable, and a policy that is not enforced is exactly the case
worth surviving.

**The value.** `profiling.thirdPartySymbols` is "drop" or "keep". It was
compared to `"keep"` and everything else took the other branch, so `"Keep"`,
`"true"` and a typo all meant `drop`. Failing closed is the right direction —
but this field governs the allow-list that decides which frames may leave the
node (ADR 0011 §4), and it is the one control whose effect an operator cannot
check by reading their own values file. `UnmarshalStrict` already rejects a
field that does not exist; nothing rejected a value that does not exist.

## Decision

A refresh happens at most once per 30 seconds. A miss inside that window
returns an error without touching the network, and the stamp is taken before
the attempt, so a refresh that failed is rate-limited on the same floor — an
unreachable API server is where retrying per request costs the most and helps
least.

Thirty seconds against the node's one-minute scan interval: a rotation is picked
up at most one round late, and a report that did not authenticate is re-sent by
the next pass rather than lost (ADR 0003). The cost is paid in the rare case,
the bound holds in the cheap one.

`Config` and `NodeConfig` gained a `Validate`, called from the loader, which
rejects a `thirdPartySymbols` outside the enumeration. Empty stays valid: it
selects the default, and it is what a default install renders.

Alternatives considered:

- **Negative-caching the unknown key IDs** instead of a time floor. It bounds
  repeats of one ID and nothing else — an attacker sends a new ID each time, and
  the cache is then a memory cost with no bound of its own.
- **Moving the fetch out from under the mutex** (single-flight). It is the fix
  for a *slow* refresh stalling verification, which is a real second problem;
  it does nothing about how often the fetch happens, which is this one.
- **Rate-limiting the endpoint** rather than the refresh. It would have to be
  set high enough for a large fleet's legitimate traffic, and the expensive
  request is indistinguishable from the cheap one at that layer.
- **Validating `thirdPartySymbols` at use** rather than at load, taking an
  unknown value as `drop` and logging it. A log line is not read on a config
  the customer set months ago; the point is that the file and the behavior
  cannot disagree.

## Consequences

The controller's own query rate to its API server is bounded by the controller.
A key rotation is picked up up to thirty seconds later than before, which costs
at most one scan round of one loss-harmless payload.

An install whose `thirdPartySymbols` is misspelled now fails to start where it
used to run with a stricter policy than the file asked for. That is a behavior
change for a configuration that was already not doing what it said, and it is
the same shape as ADR 0040's rule for an unset `expectedSubject`: a control that
cannot be honored stops the agent rather than silently becoming something else.
