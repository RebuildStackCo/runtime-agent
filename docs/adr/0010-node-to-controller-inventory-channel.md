# 0010. Node → controller inventory channel: node-initiated HTTP, projected-token auth, local JWKS validation

Date: 2026-08-10

Status: Accepted (realizes the promise in 0008 §4 point 4; extends 0009, whose
node role was log-only. The one-way boundary of 0001 and the identity model of
0005/0008 are unchanged.)

## Context

ADR 0009 gave the node role a Go-binary scanner and deliberately stopped at the
node's own log: "Delivery to the controller for aggregation is a later slice."
This is that slice. The scanner already produces exactly what the controller
cannot learn from the Kubernetes API — Go version, module path, PGO flag, tied
to a pod UID and container ID — and the controller already holds the workload
inventory (PodWatcher: namespace, owner chain, image digest) those facts need
to be joined against. Nothing is missing except a wire between them.

Three constraints from prior decisions bound the design, and none of them may
bend:

- **One-way flow (0001).** The agent initiates every connection and nothing a
  peer returns changes agent behavior. This governs the agent↔backend edge;
  the same discipline must hold one level down, on the node↔controller edge.
- **The node holds no Kubernetes identity (0009 §4).** Its ServiceAccount is
  bound to nothing and `automountServiceAccountToken: false` keeps the default
  API token out of the container. A node component that could reach the API
  server would widen the audited surface across every node.
- **Nothing durable lives on the node (0008 §4).** ADR 0008 already named the
  mechanism this slice must use: "Node pods authenticate to the controller with
  projected ServiceAccount tokens (audience-bound, kubelet-rotated, nothing
  stored on the node)." The controller stays the only identity holder and the
  only egress point.

Two forces pull against a naive "just call the API" answer. Authenticating the
node to the controller with a Kubernetes-issued token is the obvious fit — the
kubelet already mints and rotates ServiceAccount tokens — but the usual way to
*verify* such a token, `TokenReview`, is a `create` verb on
`authentication.k8s.io/tokenreviews`. Granting it to the controller would add a
write verb to a product whose single write grant (0008) was justified at
length. The token can instead be verified locally: a projected token is a
signed JWT, and the cluster publishes the public keys to verify it at its OIDC
JWKS endpoint. Local validation needs no `TokenReview` and therefore no new
RBAC verb.

## Decision

1. **The node initiates; the controller receives.** The node role POSTs its
   scan result — the kept (non-infrastructure) binaries and the aggregate
   counters, already filtered on the node (0009 §5) — to the controller over
   plain HTTP on the cluster network, addressed by the controller's in-cluster
   Service DNS. The controller runs an HTTP receiver for this and only this.
   The receiver's response carries an acknowledgment or an error and nothing
   else: it never returns configuration, tasks, or anything the node acts on.
   The node's behavior is fixed by its own flags and ConfigMap, exactly as the
   agent's is fixed against the backend (0001, backend-requirements §1). This
   is the one-way rule applied one level down — data flows node → controller →
   backend, and no peer's reply is a control channel.

2. **Authentication is a projected ServiceAccount token, audience-bound to the
   controller.** The DaemonSet mounts a `serviceAccountToken` projected volume
   with `audience` set to the controller's configured identifier and a short
   expiration; the kubelet issues and rotates it on a `tmpfs`-backed path. The
   node reads the token from that file on each send, so rotation needs no
   restart. This is the *only* credential the node holds, it is not persisted,
   and it is not the default API token.

3. **The token is not an API credential, so the node still cannot reach the API
   server.** An audience-bound token authenticates only to that audience: the
   API server rejects a token whose audience is the controller, and the node's
   ServiceAccount remains bound to zero RBAC regardless. `automountServiceAccountToken`
   stays `false`. The node gaining a controller-audience token does not give it
   a Kubernetes identity — 0009's "no API access" property is intact, now by two
   independent barriers (wrong audience *and* no grant), not one.

4. **The controller validates the token locally against the cluster JWKS — no
   `TokenReview`.** On startup the controller resolves the cluster's OIDC
   discovery document (`/.well-known/openid-configuration`) to its `jwks_uri`
   and fetches the signing keys, using its existing in-cluster REST client. It
   then verifies each incoming token itself: signature against the JWKS
   (RS256/ES256), `exp`/`nbf`, that the audience contains the controller's
   configured identifier, and that the subject is the node role's expected
   ServiceAccount. This adds **no resource RBAC verb** — in particular no
   `create` on `tokenreviews`. Reading the discovery/JWKS endpoints is a
   read-only `nonResourceURL` GET, granted cluster-wide to ServiceAccounts by
   the built-in `system:service-account-issuer-discovery` role on typical
   clusters; where a chart must grant it explicitly it is a read, consistent
   with the read-only principle, never a write.

5. **The controller joins node facts against its own inventory.** Each fact
   carries a pod UID and container ID (parsed from the process cgroup on the
   node, 0009 §2). The controller matches them against PodWatcher — pod UID →
   namespace and workload, container ID → container name and the image digest
   PodWatcher already collects — and forms one Go-inventory record per
   (namespace, workload, container): Go version, module path, image digest,
   PGO flag. Records dedup across the many replicas and nodes that run the same
   build. A fact whose pod UID or container ID is not (yet) in the inventory —
   informer lag, or a pod outside the controller's filters — is counted as
   unjoined and dropped, never guessed. The join state is in memory only: it is
   reconstructed from the next node scan and the live cluster, so it adds no
   persistent state and 0003's loss-harmless property is unaffected.

## Transport confidentiality — the honest tradeoff

The channel is plain HTTP with a bearer token, not TLS. The consequences,
stated rather than hidden:

- The token and the facts travel in cleartext on the cluster network. A party
  that can already sniff pod-to-pod traffic can read them and can replay the
  token to the controller until it expires.
- What that buys an attacker is bounded to the same blast radius as a stolen
  identity Secret (0008): they can POST fabricated Go-inventory facts for this
  one cluster — data poisoning of a medium-sensitivity metadata stream. The
  channel is one-way (the controller returns nothing to act on), carries no
  monetary fields (0004), and grants no read or control capability. The facts
  themselves were already filtered on the node, so nothing sensitive that
  wasn't destined to leave the cluster is exposed.
- Compensating controls: the audience-bound token is short-lived and rotated;
  a NetworkPolicy shipped with the chart restricts who may reach the receiver
  port to the node DaemonSet; the subject check rejects tokens that are not the
  node role's ServiceAccount.
- If a deployment needs wire confidentiality, the controller can later serve
  this endpoint over TLS with the node pinning or skip-verifying (the node
  holds no CA, 0008) — an additive change to this ADR, documented there, not a
  silent one. Plain HTTP is the starting point because the data is
  already-filtered metadata and the token is the authentication, not the
  transport.

## Consequences

Easier:

- The Go-version and module inventory reaches the controller and becomes a
  shippable payload, closing the loop 0009 opened, at no new privilege: the
  node keeps zero RBAC and no external egress, the controller adds no write
  verb and no `TokenReview`.
- Verifying identity locally means the receiver has no API-server round-trip on
  the hot path and no dependency on `TokenReview` availability.
- The mechanism is exactly the one 0008 promised, so no earlier decision has to
  be reopened; this ADR realizes a commitment rather than changing course.

Harder, or given up:

- The controller now listens on a port. It is an ingress *from inside the
  cluster only* — never from the backend or the internet — and its contract is
  "accept a data push, answer ack/error." Keeping it that and only that is a
  standing obligation; a receiver that ever influenced node behavior would
  break 0001.
- Plain HTTP exposes the token and facts on the cluster network (see the
  tradeoff above). This is accepted deliberately, with the blast radius bounded
  and the upgrade path named.
- The controller depends on the cluster's OIDC discovery/JWKS endpoints being
  reachable and on projected-token issuance being enabled (the
  `ServiceAccountTokenProjection` path, on by default for years). A cluster
  that disabled either cannot run this channel; the controller reports that
  condition and degrades to not accepting node facts, rather than failing
  closed on all collection.
- One dependency enters go.mod for JWT verification (`golang-jwt/jwt/v5`,
  pure-Go, no transitive dependencies). JWKS fetch and JWK→key conversion stay
  first-party and audited here.
