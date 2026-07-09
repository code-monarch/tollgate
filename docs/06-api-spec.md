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

### `GET /v1/analytics/services/{id}`
Revenue per route, caller cohorts, price elasticity, recommendations.

## Marketplace / discovery

### `GET /v1/services?query=…&maxPrice=…&category=…&minReputation=…`
Search the registry. Returns machine-readable entries (price, schema, SLA, reputation).

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
