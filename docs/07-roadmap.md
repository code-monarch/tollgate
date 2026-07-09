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

## Milestone 2 — Real settlement + escrow

- Wire a real stablecoin rail (USDC on Base) behind the `Settlement` interface.
- Escrow-with-verification path for agent-to-agent (`release` / `refund`).
- Receipts (signed, both parties).

## Milestone 3 — Marketplace / discovery

- `services` registry + search API.
- Reputation scoring (SLA adherence, dispute rate).
- MCP server: `search_services`, `get_service`, `call_service`.

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
