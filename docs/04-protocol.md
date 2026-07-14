# 04 — Protocol (x402 flow)

Tollgate implements the [x402](https://x402.org) standard: HTTP `402 Payment Required` is
used to negotiate a price and prove payment inline, within the normal request/response cycle.

## End-to-end flow

```
Agent           Buyer Plane        Facilitator        Seller (edge)      Seller Plane
  │ "call API X"    │                  │                   │                 │
  ├────────────────►│                  │                   │                 │
  │                 │ discover X in marketplace (price, schema)               │
  │                 │◄─────────────────────────────────────┐                 │
  │           request X ──────────────────────────────────►│                 │
  │                 │                  │       402 + quote ◄┤ pricing engine  │
  │                 │◄─────────────────────────────────────┤                 │
  │        policy check (budget? ceiling? domain? approval?)│                 │
  │                 │  ✓ authorize     │                   │                 │
  │                 ├─── pay(quote) ──►│                   │                 │
  │                 │                  │ verify + escrow   │                 │
  │           retry X + payment proof ─────────────────────►│ verify → 200   │
  │                 │                  │  settle → seller  │──► meter event ─►│
  │◄──────── response + receipt ───────┤                   │                 │
  │                 │  ledger write (both sides)            │                 │
```

The two hard, defensible steps are **the policy check** (buyer trust) and
**verify + settle + meter** (seller trust).

## Step 1 — Unpaid request → 402

Agent (or Tollgate buyer client) hits the seller resource with no payment proof. The seller
enforcement layer (Worker or SDK) calls the pricing engine and returns:

```http
HTTP/1.1 402 Payment Required
Content-Type: application/json
WWW-Authenticate: X402 realm="tollgate"

{
  "x402Version": 1,
  "accepts": [
    {
      "scheme": "exact",
      "network": "base",
      "asset": "USDC",
      "amount": "10000",              // minor units (0.01 USDC, 6 decimals)
      "payTo": "0xSELLER…",
      "resource": "https://api.example.com/geocode",
      "quoteId": "q_01H…",
      "nonce": "n_abc…",
      "expiresAt": "2026-07-08T12:00:03Z",

      // What the seller claims over the intelligence exhaust of this call.
      // Absent ⇒ it claims nothing (08-learning-boundary.md).
      "exhaust": {
        "required": [],                 // will not serve without these
        "optional": ["train"],          // would like these…
        "rebates":  { "train": "150" }  // …and pays this back, per right, if granted
      },
      "signature": "…facilitator sig…"  // covers the price AND the rights ask
    }
  ]
}
```

`accepts` is a list so a seller can offer multiple currencies/networks. The quote is
signed and time-boxed (TTL in seconds). The exhaust offer is inside the signature, so a
seller cannot widen its rights ask after the fact.

## Step 2 — Policy check (buyer side)

Before paying, the buyer plane runs the [policy engine](05-policy-engine.md) against the
quote: budget remaining, per-call ceiling, domain/category allowlist, velocity, whether
the amount crosses a human-approval threshold, and **which exhaust rights the agent may
grant**. Deterministic, logged, `< 10ms`.

- **Allow** → construct payment, granting at most what the policy permits.
- **Deny** → return a structured error to the agent (never a silent charge). A seller that
  *requires* a right the policy will not grant is denied here: no funds move, and no data
  crosses.
- **Needs approval** → fire the approval webhook, hold until resolved or timeout.

Rights are **deny-by-default**: a policy with no `exhaust_rights` rule grants nothing.

## Step 3 — Payment + retry

Buyer constructs a payment payload signed by the agent key and retries the *same* request
with proof. The payload carries the buyer's `grant` — the rights it consents to — and the
agent's signature covers it, so consent cannot be forged, widened or stripped in transit:

```http
GET /geocode?q=… HTTP/1.1
X-Payment: <base64 {quoteId, nonce, agentId, payFrom, grant:["train"], signature}>
```

## Step 4 — Verify + settle

Seller enforcement forwards the proof to the facilitator:

- **Verify** — signature valid, quote not expired, nonce unused, amount matches.
- **Rights** — the effective grant is `(required ∪ optional) ∩ grant`. If the seller
  *requires* something the buyer did not grant, settlement is **refused**: no funds move
  and no data crosses.
- **Settle** — move funds buyer → seller (peer-to-peer stablecoin). For escrowed
  agent-to-agent calls, hold in escrow and release on delivery confirmation; the granted
  rights and the dividend vest on release, never on refund.
- **Ledger** — write the double-entry pair (debit buyer, credit seller), plus the **data
  dividend** legs when rights were granted (debit seller, credit buyer). Booked gross, so
  revenue and the cost-of-knowledge stay separately auditable.

On success the seller serves `200` with the resource plus a receipt reference; the seller
plane records a **meter event** (async, off critical path). Both parties' receipts bind the
granted rights and the dividend into the facilitator's signature — a non-repudiable record
of where the learning boundary was drawn ([08-learning-boundary.md](08-learning-boundary.md)).

## Pricing models (what the pricing engine may return)

1. **Static per-route** — fixed price per verb+route (e.g. `GET /geocode` = $0.001).
2. **Variable by complexity** — price computed from request features (e.g. image gen up to
   $2 by resolution/steps). Reflected in `meter_events.units`.
3. **Dynamic** — load/demand-adjusted (surge for compute, cheaper on cache hits).

## Idempotency & replay safety

- The **quote nonce** is single-use; a second payment against it is rejected.
- The **request hash** (canonicalized method + path + body + quoteId) is the idempotency
  key on both `transactions` and `meter_events` — retries are no-ops.
- Expired quotes force a fresh 402; buyers must re-run the policy check on the new quote.

## Auth-to-402 conversion

For existing APIs that return `401`, the enforcement layer can intercept and convert to a
`402` with a quote — monetizing endpoints that previously required an account, with no code
change on the origin.

## Failure modes

| Situation | Behavior |
|-----------|----------|
| Quote expired | `402` reissued with a new quote |
| Payment verify fails | `402` with error detail; no settlement, no meter event |
| Settlement fails after verify | transaction stays `paid`, retried; resource withheld until `settled` |
| Escrow delivery not confirmed | funds returned to buyer after dispute window |
| Duplicate request hash | original result returned; no new charge |
