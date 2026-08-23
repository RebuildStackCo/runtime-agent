# 0008. Agent identity lives in a namespaced Secret — the product's one write grant

Date: 2026-08-09

Status: Accepted
Amends: 0003, 0005, 0007
Amended by: 0010, 0026

Amends 0003 and 0005 on where identity is stored; their spool/checkpoint and
two-tier-credential decisions stand. Refines the degradation notes of 0007.

## Context

ADR 0005 gives each cluster one mTLS identity whose private key is born
in-cluster and never leaves it. ADR 0003 placed that identity on the
persistent volume; ADR 0007 then made the volume an `emptyDir` by default,
arguing that data loss is bounded by minute-cadence snapshot delivery.

That argument does not transfer to identity. Data is re-derivable; a key is
not. A pod rescheduled after the enrollment token's TTL has expired cannot
re-enroll: the agent degrades to local-only and stays there until an
operator issues a new token. Node drains and upgrades are routine cluster
operations — a telemetry agent that silently dies months after installation
because of one is unacceptable, and no snapshot cadence bounds that outage.

Storing the key in a Kubernetes Secret collides with two categorical
promises in security.md: "no write verbs anywhere" and "the key is
deliberately not written to a Secret". Both were written before the
durability model changed; keeping them now costs availability of the very
telemetry the product exists to deliver.

## Decision

1. **Identity (private key and client certificate) is stored in one
   pre-created, named Secret** in the agent's own namespace. The Helm chart
   creates the Secret as a release resource with no `data` key; the agent
   generates the key and writes it there. Helm owns the object's existence,
   the agent owns only its content. The key still never leaves the cluster
   (ADR 0005).
2. **The RBAC grant is the narrowest Kubernetes allows**: a namespaced Role
   with `get` and `update` on `secrets`, restricted by `resourceNames` to
   that single Secret. No `create` — it cannot be scoped by resourceName
   (the object's name is unknown at authorization time), so granting it
   would mean write access to arbitrary Secrets in the namespace; chart
   pre-creation is what keeps the grant airtight. No `list`, `watch`,
   `delete`. No ClusterRole. This is the only write verb in the entire
   product.
3. **Strict mode remains write-free**: `persistence.enabled: true` keeps
   identity on the PVC instead, and the chart then emits no Secret grant at
   all — the pre-0008 posture, for installations that refuse any write
   access.
4. **DaemonSet profiles hold no backend identity** — a promise, not an
   accident. Node pods authenticate to the controller with projected
   ServiceAccount tokens (audience-bound, kubelet-rotated, nothing stored
   on the node). The controller is the only egress point and the only
   identity holder; identity count stays one per cluster regardless of
   node count.
5. Re-enrollment stays as the recovery of last resort (Secret deleted,
   certificate revoked) — unchanged from ADR 0005, and enrollment tokens
   stay short-lived; durable identity is what allows them to.

## Lifecycle

- **Install**: the chart creates the empty Secret, the Role scoped to it by
  name, and the RoleBinding. First agent start: `get` finds it empty →
  generate key in memory → CSR → enrollment with the operator's token →
  one `update` writes key and certificate together. A single API call, so
  no half-written identity can exist.
- **Restart / reschedule / node loss**: `get` finds the identity → resume.
  No enrollment involved.
- **Certificate renewal**: over the existing mTLS session, then `update`.
- **Upgrade**: the Secret template carries no `data`, so Helm's three-way
  merge patches nothing the agent wrote. GitOps note: ArgoCD-style
  self-heal needs the standard `ignoreDifferences` on `data` — the same
  practice cert-manager documents for its Secrets.
- **Uninstall**: the Secret belongs to the release and dies with it —
  deliberate; reinstall goes through re-enroll and the cluster's backend
  history stays continuous. No `helm.sh/resource-policy: keep`: orphaned
  key material after uninstall is worse than one extra enrollment.
- The controller is a single-replica StatefulSet: one writer, no races.
- The agent accesses the Secret via the API only; nothing mounts it — a
  volume mount could not serve `update` anyway.

## Consequences

- A rescheduled or node-lost pod resumes with its existing identity; the
  "silently local-only until an operator notices" failure class is gone.
- The RBAC table loses its categorical "no write verbs anywhere". The
  promise becomes: *no write access except get/update on its own named
  identity Secret*. Auditors verify one scoped Role instead of an absence;
  security.md discloses this in the same table that grants it.
- The key gains one copy location: etcd, and any backup that includes
  Secrets. Whoever reads it can impersonate this cluster's telemetry
  stream — data poisoning of this one cluster, nothing more: the protocol
  is one-way (nothing to read, nothing to control), monetary-free, and the
  fingerprint check stops cross-cluster mixing. Recovery is revoke +
  re-enroll with continuous history. Cluster admins gain nothing they
  could not already take from the running pod.
- Clusters without etcd encryption at rest now hold the key unencrypted in
  etcd — no worse than the unencrypted node disk or PV it lived on before,
  but stated honestly.
