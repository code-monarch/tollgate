# 07 — Roadmap

Build order follows the flywheel: **supply first**, then discovery, then demand, then
retention. Don't build all four planes at once.

## Sequencing logic

```
Seller SDK + Facilitator   →   Marketplace   →   Buyer wallet + Policy   →   Analytics
      (supply)                 (discovery)            (demand)               (retention)
```

Each stage creates the demand for the next: supply gives the marketplace something to list;
the marketplace pulls in agents; agents need the wallet + policy layer; analytics is what
makes sellers stay.

## Milestone 0 — Spec (this repo, now)

- [x] Overview, architecture, data model, protocol, policy engine, API spec.
- [ ] Agree the spec. **No service code before this is signed off.**

## Milestone 1 — The wedge: seller SDK + minimal facilitator ✅

Goal: a seller can 402-enable one Go endpoint in a few lines and get "paid" end-to-end.

- [x] Facilitator core in Go: `quotes`, `payments/verify`, `payments/settle`.
- [x] Ledger: double-entry writer + derived-balance read, behind one `Store` interface with
      two implementations — in-memory and Postgres (`db/schema.sql`, `internal/ledger/pgstore.go`).
- [x] **Settlement mocked** behind the `Settlement` interface (no real chain yet) — prove the
      protocol before wiring money.
- [x] Seller SDK (Go `net/http` middleware) implementing the x402 flow.
- [x] One runnable example: a priced `GET /geocode` guarded by the SDK (`cmd/demo`).

Exit ✅: unpaid request → 402 → paid retry → 200 + ledger entries, with idempotent retries.
Verified by `cmd/demo` and `internal/facilitator` end-to-end tests (in-process and over HTTP),
plus a Postgres store integration test (`scripts/pgtest.ps1`) covering idempotency under
concurrent writes.

## Milestone 2 — Real settlement + escrow ✅

- [x] Real stablecoin rail wired behind a `rail.Rail` interface — **Bitnob**
      (`POST /api/withdrawals`, HMAC-SHA256 request signing) with a mock fallback
      (`internal/rail`, `internal/rail/bitnob`).
- [x] Escrow-with-verification for agent-to-agent: `settle(escrow)` holds funds in
      an escrow account; `release` pays the seller, `refund` returns to the buyer.
      Both idempotent (`internal/facilitator/escrow.go`).
- [x] Receipts — signed (facilitator ed25519) for both buyer and seller, verifiable
      and retrievable (`internal/receipt`, `GET /v1/receipts/{tx}`).
- [x] Payout + async webhook finalization: `POST /v1/payouts` reserves funds and
      sends over the rail; `POST /v1/webhooks/bitnob` (HMAC-verified) settles or
      reverses on `transfer.success`/`transfer.failed`.

**Design note — where money touches the chain.** Per-call buyer→seller settlement
is **custodial and internal**: the double-entry ledger *is* the settlement. On-chain
per-call transfer is a non-starter for $0.001 micro-payments (gas ≫ payment). The
real stablecoin rail (Bitnob) is used at the edges — **payouts** (a seller withdraws
an accrued internal balance to a stablecoin address) and, later, funding deposits.
This keeps the hot path sub-second and cheap while still settling in real stablecoin.

The Bitnob client is written to the documented contract and tested against a mock
server; moving real funds needs live credentials + a sandbox run.

## Milestone 3 — Marketplace / discovery ✅

- [x] `services` registry + search API — filter by text/category/max-price/min-reputation,
      ranked by reputation (`internal/registry`, `internal/marketplace`, `cmd/marketplace`).
- [x] Reputation scoring from SLA adherence + dispute rate — explainable, no ML on any
      path; seeds from advertised uptime before the first call (`reputation.go`).
- [x] MCP server: `search_services`, `get_service`, `call_service` over JSON-RPC/stdio.
      `call_service` runs the full x402 flow via a buyer client and returns result +
      receipt (`internal/marketplace/mcp.go`, `caller.go`).

Verified end to end: an agent discovers a service via the MCP `call_service` tool, the
x402 flow pays the seller through the ledger, and the result + receipt return to the
caller (`internal/marketplace` e2e test).

## Milestone 4 — Buyer wallet + policy engine

- Agent identity + wallet + funding.
- Policy engine (all rule types from `05-policy-engine.md`), `/authorize` + `/pay`.
- Human-approval webhook flow.

## Milestone 5 — Analytics + dynamic pricing

- Revenue/route, caller cohorts, elasticity.
- Pricing recommendations; dynamic pricing engine.

## Cross-cutting (every milestone)

- Idempotency + replay safety on anything that charges.
- Structured audit logging of every policy decision and settlement.
- No float money; double-entry invariant enforced in tests.

## Open questions

- Custodial vs MPC wallets for buyers at launch (custodial is faster; MPC is the pitch).
- Which x402 facilitator stack to sit on first vs. running our own verifier.
- Fiat payout partners per region (this is where local rails/FX matter).
- Pricing of Tollgate itself: take-rate on settlement vs. flat platform fee vs. both.
