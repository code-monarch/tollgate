# Tollgate

**The commerce layer for the agent economy.**

Tollgate sits between AI agents and the paid resources they call. Every request that
crosses it is metered, priced, authorized against a policy, paid via [x402](https://x402.org),
settled to the seller, and logged for both sides.

It unifies four capabilities that are usually pitched as separate products into one
two-sided platform:

| Face | Side | What it is |
|------|------|-----------|
| Wallet + spend controls | **Buyer** | Agent identity, funded balance, policy engine, receipts |
| Metering & pricing engine | **Seller** | Per-route metering, dynamic pricing, revenue analytics |
| x402 facilitator | **Rail** | Quote → verify → settle, stablecoin ↔ fiat |
| Marketplace | **Discovery** | Registry of paid endpoints, reputation, runtime search |

Buyers reinforce sellers and vice-versa — a classic two-sided flywheel with the
transaction rail in the middle.

## Why now

The web was monetized by trading content for human attention (ads, subscriptions).
AI agents don't view ads or hold subscriptions, and they make hundreds to thousands of
requests per human. The unlock is **HTTP 402 "Payment Required"** made real and enforced,
built on the x402 standard: payment becomes part of the request/response cycle, and
**payment itself is the credential** — no signup, no API key, no prior relationship.

## Repo map

```
tollgate/
├── docs/                 # design spec suite (start here)
│   ├── 01-overview.md          vision, principles, glossary
│   ├── 02-architecture.md      system + component design
│   ├── 03-data-model.md        entities, tables, invariants
│   ├── 04-protocol.md          x402 flow: quote → verify → settle
│   ├── 05-policy-engine.md     buyer-side authorization rules
│   ├── 06-api-spec.md          HTTP surface for all planes
│   └── 07-roadmap.md           build order + milestones
└── (services scaffolded after the spec settles)
```

## Status

Spec agreed. **Milestones 1–4 are implemented in Go.**

- **M1 — the wedge**: a minimal facilitator (quote → verify → settle), an append-only
  double-entry ledger with derived balances (in-memory + Postgres), and a `net/http`
  seller SDK. Exit criterion holds end to end: *unpaid → 402 + signed quote → paid
  retry → 200 + ledger entries*, idempotent.
- **M2 — real settlement + escrow**: a real stablecoin rail (**Bitnob**) behind a
  swappable `rail.Rail` interface for **payouts**; **escrow** with `release`/`refund`
  for agent-to-agent; and **signed receipts** for both parties. Per-call settlement
  stays custodial/internal (the ledger is the settlement) — see the design note in
  [docs/07-roadmap.md](docs/07-roadmap.md).
- **M3 — marketplace/discovery**: a **service registry** with search
  (text/category/price/reputation), **reputation** scoring from SLA + dispute rate, and
  an **MCP server** (`search_services`, `get_service`, `call_service`) so agents discover
  and pay for services the way they use tools. `call_service` runs the full x402 flow.
- **M4 — buyer wallet + policy engine** (the buyer-side moat): a custodial **buyer plane**
  (agents, wallets, funding) and a deterministic, deny-by-default **policy engine** — amount
  ceilings, task/rolling budgets, allow/blocklists, velocity, price-spike anomaly, and
  human-approval. `/authorize` gates every payment; `/pay` signs + settles only if allowed.

## Build & run (Milestone 1)

Requires Go 1.23+.

```sh
go test ./...          # unit + end-to-end tests (in-process and over HTTP)
go run ./cmd/demo      # prints the full 402 dance in one process
go run ./cmd/facilitator   # standalone facilitator on :8080 (in-memory ledger)
go run ./cmd/marketplace       # marketplace HTTP API on :8081
go run ./cmd/marketplace -mcp  # marketplace as an MCP server over stdio
go run ./cmd/buyer             # buyer plane (wallets + policy engine) on :8082

# Postgres-backed ledger: point the facilitator at a DB with db/schema.sql applied
DATABASE_URL=postgres://user:pass@host:5432/db go run ./cmd/facilitator

# Real stablecoin payouts via Bitnob (falls back to a mock rail when unset)
BITNOB_CLIENT_ID=... BITNOB_SECRET=... BITNOB_WEBHOOK_SECRET=... go run ./cmd/facilitator

# Verify the Postgres store against a throwaway DB in Docker
./scripts/pgtest.ps1
```

`go test ./...` needs no infrastructure; the Postgres store test is skipped unless
`TOLLGATE_TEST_DB` points at a schema-applied database (`scripts/pgtest.ps1` sets it up).

### Code map

```
x402/                  # protocol wire types + ed25519 quote/payment/receipt signing (public)
sdk/go/                # seller SDK: Guard net/http middleware + facilitator client
internal/facilitator/  # rail: quote/verify/settle + escrow + payout + webhook + HTTP
internal/ledger/       # append-only double-entry store (Store iface + in-memory & Postgres impls)
internal/settlement/   # internal (custodial) per-call settlement interface + mock
internal/rail/         # external stablecoin rail interface + mock ...
internal/rail/bitnob/  # ... and the real Bitnob rail (HMAC-signed /api/withdrawals)
internal/receipt/      # signed, verifiable transaction receipts
internal/registry/     # marketplace catalog: service entries, search, reputation
internal/marketplace/  # marketplace HTTP API + MCP server (+ buyer-driven call_service)
internal/policy/       # buyer-side moat: policy engine, rule types, tracker, approvals
internal/buyerplane/   # custodial buyer plane: agents/wallets/policies + authorize/pay
internal/money/        # integer minor-units Money (never a float)
internal/buyer/        # minimal buyer client for the demo/tests
cmd/demo/              # end-to-end walkthrough
cmd/facilitator/       # standalone facilitator server
cmd/marketplace/       # marketplace server (HTTP, or -mcp for stdio MCP)
cmd/buyer/             # buyer-plane server (wallets + policy engine)
db/schema.sql          # Postgres ledger schema (target for the Postgres Store)
```

Settlement is mocked behind the `Settlement` interface — the protocol is proven before
money is wired (real USDC-on-Base settlement is Milestone 2). The ledger ships two `Store`
implementations behind one interface: in-memory (default, used by the demo and tests) and
Postgres (`db/schema.sql`), selected at the facilitator via `DATABASE_URL`.

## Start here

Read [`docs/01-overview.md`](docs/01-overview.md), then the architecture and protocol docs.
