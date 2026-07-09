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

Spec agreed. **Milestone 1 (the wedge) is implemented in Go**: a minimal facilitator
(quote → verify → settle), an append-only double-entry ledger with derived balances,
mock settlement behind a swappable interface, and a `net/http` seller SDK. The exit
criterion holds end to end: *unpaid request → 402 + signed quote → paid retry → 200 +
ledger entries*, with idempotent retries that never double-charge.

## Build & run (Milestone 1)

Requires Go 1.23+.

```sh
go test ./...          # unit + end-to-end tests (in-process and over HTTP)
go run ./cmd/demo      # prints the full 402 dance in one process
go run ./cmd/facilitator   # standalone facilitator on :8080 (in-memory ledger)

# Postgres-backed ledger: point the facilitator at a DB with db/schema.sql applied
DATABASE_URL=postgres://user:pass@host:5432/db go run ./cmd/facilitator

# Verify the Postgres store against a throwaway DB in Docker
./scripts/pgtest.ps1
```

`go test ./...` needs no infrastructure; the Postgres store test is skipped unless
`TOLLGATE_TEST_DB` points at a schema-applied database (`scripts/pgtest.ps1` sets it up).

### Code map

```
x402/                  # protocol wire types + ed25519 quote/payment signing (public)
sdk/go/                # seller SDK: Guard net/http middleware + facilitator client
internal/facilitator/  # rail: quote / verify / settle core + HTTP handlers
internal/ledger/       # append-only double-entry store (Store iface + in-memory & Postgres impls)
internal/settlement/   # Settlement interface + mock rail
internal/money/        # integer minor-units Money (never a float)
internal/buyer/        # minimal buyer client for the demo/tests
cmd/demo/              # end-to-end walkthrough
cmd/facilitator/       # standalone facilitator server
db/schema.sql          # Postgres ledger schema (target for the Postgres Store)
```

Settlement is mocked behind the `Settlement` interface — the protocol is proven before
money is wired (real USDC-on-Base settlement is Milestone 2). The ledger ships two `Store`
implementations behind one interface: in-memory (default, used by the demo and tests) and
Postgres (`db/schema.sql`), selected at the facilitator via `DATABASE_URL`.

## Start here

Read [`docs/01-overview.md`](docs/01-overview.md), then the architecture and protocol docs.
