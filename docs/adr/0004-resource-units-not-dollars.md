# 0004. Resource units are the truth; dollars are rendering

Date: 2026-07-28
Status: Accepted

## Context

The product's findings ultimately get judged against money — but the agent
cannot know prices. Cloud pricing varies by provider, region, commitment
level, and negotiated discounts; it changes over time; and a dollar figure
computed in-cluster from a stale or wrong price list is worse than no figure,
because it destroys trust the first time it disagrees with an invoice.

Resource quantities, by contrast, are facts the agent can measure.

## Decision

- The wire protocol has no monetary fields. The agent reports resource units
  only: core-hours, GiB-hours, counts, ratios.
- Pricing is applied backend-side at render time, from a versioned pricing
  snapshot — every dollar figure is attributable to the price data that
  produced it.
- Dollar figures are shown to customers only when they can be reconciled
  against real invoices; until then, findings are presented in resource
  units.

## Consequences

- The agent needs no price feeds, no currency handling, no per-customer
  billing context — and never has to be redeployed because a price changed.
- Historical data can be re-priced retroactively when pricing data improves;
  nothing is baked in at collection time.
- The backend owns the entire "what does this cost" problem, including its
  hardest part: earning the right to show a dollar sign.
