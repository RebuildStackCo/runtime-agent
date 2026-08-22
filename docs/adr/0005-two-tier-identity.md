# 0005. Two-tier credentials with in-cluster key generation

Date: 2026-07-28
Status: Accepted
Amended by: 0008

## Context

Onboarding must scale to fleets ("50 clusters, via Terraform") without a
credential that, if leaked from any one cluster, compromises the whole
organization. Long-lived shared secrets inside clusters fail that test.
Manual per-cluster certificate ceremonies fail the scale test.

## Decision

Credentials form two tiers, and private keys are born in-cluster:

1. **Org API key** — long-lived, scoped, revocable; authorizes automation
   (Terraform, CLI) against the control plane. It never enters a cluster.
2. **Per-cluster enrollment token** — short-TTL, issued by the control plane
   for an existing cluster record, delivered to the cluster via Helm value or
   a pre-created Secret. Reusable within its TTL, so pod restarts during
   rollout do not strand the install.

At first start the agent generates a keypair on its volume (the key never
leaves the cluster), sends a CSR authenticated by the enrollment token, and
receives a client certificate carrying the cluster identity. All subsequent
traffic is mTLS against the backend's pinned private CA. Certificates renew
over the existing mTLS session without operator involvement.

Enrollment includes a cluster fingerprint (the UID of `kube-system`); the
backend rejects a fingerprint that contradicts the cluster record, so two
clusters can never silently share an identity. Re-enrollment for the same
record revokes the previous certificate and keeps data history continuous.

If identity fails entirely (revoked, expired beyond renewal), the agent
degrades to local-only operation: it keeps collecting and spooling, ships
nothing, and reports the condition in its own status.

## Consequences

- Blast radius of a leaked in-cluster credential is one cluster's short-TTL
  token or one cluster's certificate — both individually revocable.
- Onboarding is `terraform apply`-shaped: create cluster records, issue
  tokens, install charts; idempotent by cluster name.
- The backend must run a private CA with a published rotation/overlap policy
  (`docs/backend-requirements.md` §2–3) — a real operational commitment.
- CA pinning means TLS-intercepting proxies must exempt the agent's egress
  domain; this is documented as an explicit install prerequisite.
