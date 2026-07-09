# 01 — Overview

## The one-product thesis

Tollgate is **one product with four faces**, not four products. The value comes from
owning the transaction end to end: when a buyer (an agent) pays a seller (an API, dataset,
or MCP tool), Tollgate is the party that prices it, authorizes it, moves the money, and
records it for both sides.

- **Buyers** get a wallet and, more importantly, **spend controls** — budgets, price
  ceilings, allowlists, anomaly detection, human-approval thresholds. This is the real
  reason an org lets an autonomous agent hold funds at all.
- **Sellers** get **metering + pricing + analytics** — the ability to charge per call,
  price dynamically, and understand who's paying and what to charge.
- The **facilitator** is the rail connecting them: quote, verify, settle, with stablecoin
  settlement and fiat payout.
- The **marketplace** is discovery: a machine-readable registry so agents can find and
  price services at runtime.

## Principles

1. **Payment is the credential.** No signup, no API key, no prior relationship required to
   transact. Identity is layered on top for accountability, not as a gate.
2. **The edge is not the moat — the trust is.** The two defensible surfaces are the
   buyer-side **policy check** and the seller-side **verify + settle + meter**. Everything
   else is commodity plumbing.
3. **Escape the single-edge trap.** Sellers on Cloudflare get Workers-native enforcement;
   everyone else gets a drop-in SDK. Tollgate must never be "just a Cloudflare feature."
4. **Money code is strict code.** Deterministic, idempotent, double-entry, replay-safe.
   No floating-point money, no non-idempotent charges, no silent retries.
5. **Agents are first-class users.** Every capability (including the marketplace itself) is
   exposed as something an agent can call — ideally as an MCP tool.

## Who it's for

- **Sellers**: API providers, data/content publishers, and MCP tool authors who want
  usage-based revenue from machine traffic they currently can't monetize.
- **Buyers**: teams running autonomous agents that need to pay for data/compute/tools
  without a human in the loop for every transaction — but with hard guardrails.

## Glossary

- **x402** — open standard using HTTP 402 to negotiate and prove payment inline.
- **Facilitator** — the party that issues quotes, verifies payment proofs, and settles.
- **Quote** — a signed, time-boxed price challenge returned with a 402.
- **Policy** — a versioned rule set evaluated before releasing a buyer's funds.
- **Meter event** — one billable unit recorded on the seller side, idempotent per request.
- **Settlement** — moving funds (stablecoin) from buyer to seller; optionally escrowed.
- **Receipt** — a signed proof of a completed transaction, held by both parties.

See [`02-architecture.md`](02-architecture.md) next.
