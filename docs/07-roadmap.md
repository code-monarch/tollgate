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

## Milestone 4 — Buyer wallet + policy engine ✅

- [x] Agent identity + wallet + funding — custodial buyer plane mints keypairs, registers
      agents with the facilitator, funds and reads derived balances (`internal/buyerplane`).
- [x] Policy engine — all rule types from `05-policy-engine.md` (amount_ceiling, budget by
      task/rolling-window, allow/blocklist with host globs, velocity, anomaly/price-spike,
      approval), deny-by-default, most-restrictive-wins, versioned policies, deterministic
      (`internal/policy`).
- [x] `/authorize` (decision only) + `/pay` (authorize → sign with the custodied key →
      settle through the seller → record spend). No policy ⇒ deny.
- [x] Human-approval webhook flow — needs_approval parks a request, fires the webhook, and
      is unlocked by resolve; on_timeout posture enforced (`approval.go`).

Verified end to end: a policy denies an over-ceiling or budget-exhausted payment (no charge),
allows within-limits calls (seller paid via the ledger), and gates a high-value call through
needs_approval → resolve → pay (`internal/buyerplane` tests).

## Milestone 5 — Analytics + dynamic pricing ✅

- [x] Analytics read directly off the append-only ledger (`Store.Transactions`,
      a read-only seam over the same `transactions` the payment flow writes — no
      parallel metrics store to drift): revenue/route, caller cohorts, and a
      per-price-point demand table (`internal/analytics`).
- [x] Price elasticity fitted by ordinary least squares on log(quantity) vs
      log(price) across observed price points — explainable, deterministic, no ML,
      matching the reputation model's ethos. Falls back to "insufficient
      variation" until two distinct prices exist.
- [x] Pricing recommendation: a bounded (±15%) revenue-maximizing move derived
      from the elasticity — cut when elastic, raise when inelastic, hold near
      unit-elastic — with a plain-language rationale and projected revenue lift.
- [x] Dynamic pricing engine (`internal/pricing`): static / variable (banded
      tiers) / dynamic (continuous surge from demand vs a target rate), floor+
      ceiling bounded, a pure and deterministic function of (model, signals).
- [x] `GET /v1/analytics/services/{id}` + `GET /v1/pricing/services/{id}` served
      by the marketplace binary over a shared ledger seeded with demo traffic.

Verified end to end: real payments settle through the facilitator, then the
report read off the same ledger shows the seller's exact accrued revenue, the
elastic price-cut recommendation, and a demand-driven dynamic surge — over both
the pure API and HTTP (`internal/analytics` e2e test), plus unit tests for the
elasticity math and every pricing branch.

## Milestone 6 — The learning boundary (exhaust rights + data dividend) ✅

The answer to the **Reverse Information Paradox**: a firm should be able to *use* a
model without surrendering the knowledge that makes it unique. Full rationale and
invariants in [08-learning-boundary.md](08-learning-boundary.md).

- [x] **Rights are protocol, not terms-of-service.** The seller's exhaust ask
      (`retain`, `train`, `distill`, `share_third_party`, `human_review`,
      `improve_memory`; required vs optional, with a rebate per right) rides inside
      the **facilitator-signed quote**; the buyer's grant rides inside the
      **agent-signed payment**. Neither side can widen, strip or rewrite it
      (`internal/rights`, `x402`).
- [x] **Deny by default.** The `exhaust_rights` policy rule declares what an agent
      may *ever* grant. No rule ⇒ nothing is grantable — enforced centrally, so
      silence never grants. A seller that *requires* rights the policy refuses is
      **denied**: no funds move, no data crosses (`internal/policy`).
- [x] **The exhaust is priced — the data dividend.** Granting rights earns a rebate
      the seller pays back, settled as explicit legs in the double-entry ledger and
      booked **gross**, so revenue and the cost-of-knowledge stay separately
      auditable. Clamped to the price: a call can be free, never negative.
- [x] **Rights-bearing receipts.** The effective grant and dividend are bound into
      the facilitator-signed receipt for both parties — Arrow's patent-equivalent,
      pointed the other way: non-repudiable proof of what was disclosed, granted,
      refused, and paid for (`internal/receipt`).
- [x] **Shop the boundary.** `GET /v1/services?excludeRequired=train` finds models
      that will not demand the right to learn from you. Choosing a model on its
      terms toward your knowledge is a search, not a legal review.

Verified end to end: a seller demanding training rights is **denied** under a policy
that grants none (not a cent moves); an optional ask **declined** pays full list
price with a receipt attesting nothing crossed; an ask **granted** pays the firm its
dividend and records exactly which rights crossed; a buyer cannot grant more than was
asked; a dividend can never exceed the price; and a replay re-grants nothing
(`internal/facilitator/boundary_test.go`, `internal/buyerplane/boundary_test.go`).

## Cross-cutting (every milestone)

- Idempotency + replay safety on anything that charges.
- Structured audit logging of every policy decision and settlement.
- No float money; double-entry invariant enforced in tests.

## Open questions

- Custodial vs MPC wallets for buyers at launch (custodial is faster; MPC is the pitch).
- Which x402 facilitator stack to sit on first vs. running our own verifier.
- Fiat payout partners per region (this is where local rails/FX matter).
- Pricing of Tollgate itself: take-rate on settlement vs. flat platform fee vs. both.
