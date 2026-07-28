# 0001. Agents are never controlled from the server

Date: 2026-07-28
Status: Accepted

## Context

The agent runs inside customer Kubernetes clusters, often behind an enterprise
security review. Any channel through which a remote party can change what
software does inside the cluster — push configuration, adjust collection
targets, trigger actions — is the single most expensive item in such a review,
and a standing risk: a compromised backend would become a compromise of every
customer cluster.

At the same time, the product only needs data to flow one way. Analysis,
cost modeling, and reporting all happen outside the cluster.

## Decision

The agent initiates every connection; the backend never addresses the agent.
Responses to agent requests carry only protocol outcomes: acknowledgments,
errors, issued certificates. If a response contains anything else, the agent
ignores it by design.

All agent behavior — filters, install profile, collection settings — is
defined in-cluster via Helm values rendered into a ConfigMap. There is no
dynamic configuration from the backend, no feature flags served remotely,
no kill switch.

## Consequences

- The security review story is short: "the agent cannot be commanded; here is
  the full list of what it sends" — see `docs/security.md`.
- Configuration changes require a redeploy. This is intentional: every change
  in agent behavior is visible in Git history and cluster audit logs.
- The backend must be designed around this constraint from day one
  (`docs/backend-requirements.md` §1); no future feature may reintroduce a
  control channel.
- Fleet-wide behavior changes roll out at the customer's pace, not ours.
