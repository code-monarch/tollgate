# 06 — API Spec

HTTP surface for each plane. All money amounts are strings of integer minor units + a
currency code. All endpoints are versioned under `/v1`. Auth for management APIs is standard
bearer tokens (org-scoped); the *payment* flow uses x402 (no token).

## Facilitator (rail)

### `POST /v1/quotes`
Issue a signed quote. Called by the seller enforcement layer.

```jsonc
// request
{ "serviceId": "svc_…", "amount": "10000", "currency": "USDC",
  "network": "base", "payTo": "0xSELLER…", "resource": "https://…" }
// response 201
{ "quoteId": "q_…", "nonce": "n_…", "expiresAt": "…", "signature": "…" }
```

### `POST /v1/payments/verify`
Verify a payment proof against a quote. Returns validity without settling.

```jsonc
{ "quoteId": "q_…", "proof": "<x-payment payload>" }
// → { "valid": true, "agentId": "agt_…", "amount": "10000" }
```

### `POST /v1/payments/settle`
Verify (if not already) and settle funds buyer → seller; writes the ledger pair.

```jsonc
{ "quoteId": "q_…", "proof": "<…>", "escrow": false }
// → { "transactionId": "txn_…", "status": "settled", "receiptId": "rcpt_…" }
```

### `POST /v1/escrow/{transactionId}/release`  ·  `/refund`
Release escrowed funds to the seller on delivery confirmation, or refund the buyer after the
dispute window.

## Buyer plane

### `POST /v1/agents`
Create an agent identity (keypair bound).

### `POST /v1/agents/{id}/wallet/fund`
Fund the agent wallet (stablecoin deposit reference).

### `GET /v1/agents/{id}/balance`
Derived balance from the ledger.

### `POST /v1/policies`  ·  `GET /v1/policies/{scope}`
Create a new policy version / fetch the active policy. Schema in
[`05-policy-engine.md`](05-policy-engine.md).

### `POST /v1/authorize`
Run the policy check against a quote **without paying** — the buyer client calls this between
receiving a 402 and paying.

```jsonc
{ "agentId": "agt_…", "taskId": "task_…", "quote": { … } }
// → { "decision": "allow" | "deny" | "needs_approval",
//     "firedRules": ["per-call-ceiling"], "approvalRequestId": "apr_…"? }
```

### `POST /v1/pay`
Convenience: authorize + construct payment + settle in one call for the buyer client.

## Seller plane

### `POST /v1/services`
Register a sellable service (also lists it in the marketplace).

```jsonc
{ "name": "Geocoder", "endpoint": "https://api.example.com/geocode",
  "pricingModel": { "type": "static", "amount": "1000", "currency": "USDC" },
  "schema": { … }, "sla": { "uptime": 0.999 } }
```

### `PUT /v1/services/{id}/pricing`
Update the pricing model (static / variable / dynamic).

### `POST /v1/meter`
Record a billable event (idempotent on `requestHash`). Usually called by the SDK, not humans.

```jsonc
{ "serviceId": "svc_…", "transactionId": "txn_…",
  "requestHash": "…", "units": 1 }
// → 200 { "recorded": true }   (duplicate requestHash → { "recorded": false })
```

### `GET /v1/analytics/services/{id}?since=…`
Revenue per route, caller cohorts, price elasticity, recommendations, computed off
settled ledger transactions. `since` is optional (RFC3339 timestamp or a duration
like `24h`; defaults to a 30-day lookback).

```jsonc
// → { "serviceId": "svc_…", "revenue": 24000, "calls": 22, "uniqueCallers": 2,
//     "pricePoints": [ { "price": 1000, "calls": 18, "revenue": 18000 }, … ],
//     "cohorts": [ { "agentId": "agt_…", "calls": 16, "spend": 18000 }, … ],
//     "elasticity": { "coefficient": -3.71, "method": "log-log-ols", … },
//     "recommendation": { "recommendedPrice": 850, "expectedRevenueLift": 0.55, … } }
```

### `GET /v1/pricing/services/{id}?window=…`
Resolve the price to quote right now for a service, from its pricing model and the
settled-call count in the demand `window` (default `1h`). Static returns the base;
variable/dynamic surge or discount around it.

```jsonc
// → { "price": 600, "currency": "USDC", "model": "dynamic", "surge": 0.2,
//     "rationale": "dynamic: 14 call(s)/1h vs target 10 → +20% surge → 600 USDC" }
```

## Marketplace / discovery

### `GET /v1/services?query=…&maxPrice=…&category=…&minReputation=…&excludeRequired=…`
Search the registry. Returns machine-readable entries (price, schema, SLA, reputation,
and the service's **exhaust terms**).

`excludeRequired` is a comma-separated list of exhaust rights: any service that *requires*
one of them is dropped. This is how a firm shops its own learning boundary — finding a
model that will not demand the right to train on you is a search, not a legal review
(docs/08-learning-boundary.md).

```jsonc
// GET /v1/services?excludeRequired=train
// A listed service advertises what it claims over your intelligence exhaust:
// { "id": "svc_…", "pricing": { … },
//   "exhaust": { "optional": ["train","retain"],
//                "rebates":  { "train": 150, "retain": 25 } } }   // it PAYS to learn from you
```

### MCP server
The marketplace is also exposed as an MCP server so agents discover services the same way
they discover tools:

- `search_services(query, constraints)` → candidate services
- `get_service(id)` → schema + pricing + SLA
- `call_service(id, args)` → runs the full 402 flow (authorize → pay → invoke) and returns
  the result + receipt

## Enforcement SDK (seller-side, in-process)

Not an HTTP API — a library. Pseudocode of the middleware contract:

```go
tollgate.Guard(tollgate.Config{
    ServiceID: "svc_…",
    Pricing:   tollgate.Static("1000", "USDC"),
    Facilitator: facilitatorURL,
})
// On unpaid request → 402 + quote. On paid retry → verify+settle, then next handler,
// then meter event (async).
```
