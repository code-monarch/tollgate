# 02 — Architecture

## System sketch

```
                        ┌──────────────────────────────────────────┐
                        │              TOLLGATE PLATFORM             │
                        │                                            │
   AI Agent /           │   ┌───────────────┐   ┌───────────────┐   │
   Agent framework ─────┼──►│  BUYER PLANE   │   │  SELLER PLANE  │  │
   (LangChain, MCP,     │   │  • Agent ID    │   │ • Metering     │  │
   custom)              │   │  • Wallet/bal. │   │ • Pricing eng. │  │
        ▲               │   │  • Policy eng. │   │ • Analytics    │  │
        │               │   │  • Receipts    │   │ • Payout acct  │  │
        │               │   └───────┬───────┘   └───────▲───────┘   │
        │               │           │                   │           │
        │               │   ┌───────▼───────────────────┴───────┐   │
        │  402 + quote   │   │        FACILITATOR (rail)         │   │
        └───────────────┼──►│  quote → verify → settle → log    │   │
                        │   │  stablecoin escrow + FX to fiat   │   │
                        │   └───────┬───────────────────────────┘   │
                        │           │                               │
                        │   ┌───────▼───────┐   ┌───────────────┐   │
                        │   │  MARKETPLACE  │   │  LEDGER / TX   │   │
                        │   │  registry +   │   │  double-entry  │   │
                        │   │  reputation   │   │  event stream  │   │
                        │   └───────────────┘   └───────────────┘   │
                        └───────────────┬──────────────────────────┘
                                        │  x402 (HTTP 402)
                                        ▼
                          ┌──────────────────────────────┐
                          │  Seller origin / API / data / │
                          │  MCP tool (Cloudflare edge or  │
                          │  Tollgate SDK middleware)      │
                          └──────────────────────────────┘
```

## Services

### Buyer plane
- **Agent Identity** — each agent gets a DID/keypair; the wallet is bound to it. Payment is
  the credential, but Tollgate always knows *which* agent paid.
- **Policy Engine** — the moat. Evaluates every intended payment against rules *before*
  releasing funds: per-task budget, per-call ceiling, domain/category allowlist, rate caps,
  velocity/anomaly checks, and a human-approval webhook above a threshold. Deterministic,
  target < 10ms, fully logged. Spec in [`05-policy-engine.md`](05-policy-engine.md).
- **Wallet** — funded stablecoin balance. Custodial (MPC) to start; non-custodial later.
  Sub-balances per agent and per task for clean accounting.

### Seller plane
- **Metering** — counts billable events per route/verb/complexity. Idempotent: deduped on
  request hash so retries never double-charge.
- **Pricing Engine** — resolves a price per request: static per-route, variable-by-complexity,
  or dynamic (load/demand). Emits the quote that goes into the 402.
- **Analytics** — revenue per route, caller cohorts, price elasticity, pricing recommendations.

### Rail — Facilitator
- **Quote** — signs a priced, time-boxed 402 challenge (nonce, TTL).
- **Verify** — validates the payment proof from the retried request.
- **Settle** — peer-to-peer stablecoin to the seller wallet; optional
  **escrow-with-verification** for agent-to-agent (release on delivery + dispute window).
- **FX / Payout** — convert stablecoin → fiat rails on withdrawal.

### Shared
- **Ledger** — append-only, double-entry; source of truth for balances, disputes, invoices,
  tax. Spec in [`03-data-model.md`](03-data-model.md).
- **Marketplace / Registry** — machine-readable catalog of paid endpoints (price, schema,
  SLA, reputation), searchable by agents at runtime. Exposed as an MCP server itself.

## Enforcement points

Two ways a seller enforces payment:

1. **Cloudflare Worker** — for sellers already on Cloudflare; native edge enforcement.
2. **Tollgate SDK / middleware** — drop-in for Express, FastAPI, Next.js, Go `net/http`.
   This is how Tollgate stays edge-agnostic.

Both speak the same x402 flow to the facilitator (see [`04-protocol.md`](04-protocol.md)).

## Trust boundaries

- The **facilitator** is trusted by both sides to verify and settle honestly; it holds no
  buyer funds longer than a transaction (escrow is explicit and time-boxed).
- The **policy engine** is trusted by the *buyer's org* to never release funds outside the
  rules. It is the last gate before money moves.
- The **ledger** is the arbiter in any dispute; every plane writes to it, none can rewrite it.

## Latency budget (single paid call)

| Step | Target |
|------|--------|
| Quote (pricing engine) | < 15ms |
| Policy check (buyer) | < 10ms |
| Verify + settle | < 500ms (sub-second goal) |
| Meter write (async) | off critical path |

## Technology

- **Facilitator, policy engine, ledger**: Go (latency + money → strict, boring, fast).
- **Seller SDK**: TypeScript first, then Python and Go.
- **Ledger store**: Postgres (double-entry) + append-only event log (Kafka/Redpanda) → analytics.
- **Settlement**: USDC / stablecoin via an established x402 stack behind a `Settlement`
  interface so rails are swappable.
- **Marketplace API**: also exposed as an MCP server — Tollgate is a tool that finds tools.
