# 03 — Data Model

Source of truth is Postgres. Money is **never** a float — integer minor units + currency
code. The ledger is append-only; balances are derived, never mutated in place.

## Entities

### `agents`
Buyer-side identity.

| column | type | notes |
|--------|------|-------|
| id | uuid PK | |
| owner_org_id | uuid FK | the org/human accountable for this agent |
| public_key | text | DID / keypair; used to bind payments |
| label | text | human-readable |
| status | enum | `active`, `suspended`, `revoked` |
| created_at | timestamptz | |

### `wallets`
One per agent (and one per seller for payouts).

| column | type | notes |
|--------|------|-------|
| id | uuid PK | |
| owner_type | enum | `agent`, `seller`, `org` |
| owner_id | uuid | |
| currency | char(3)/text | e.g. `USDC` |
| created_at | timestamptz | |

Balances are **derived from `ledger_entries`**, not stored on the wallet.

### `policies`
Buyer-side authorization rules. Versioned; attached to an agent or org.

| column | type | notes |
|--------|------|-------|
| id | uuid PK | |
| scope_type | enum | `agent`, `org` |
| scope_id | uuid | |
| version | int | monotonic per scope |
| rules | jsonb | see [`05-policy-engine.md`](05-policy-engine.md) |
| active | bool | one active version per scope |
| created_at | timestamptz | |

### `services`
Marketplace registry entry — one per sellable endpoint/tool.

| column | type | notes |
|--------|------|-------|
| id | uuid PK | |
| seller_id | uuid FK | |
| name | text | |
| endpoint | text | URL or MCP tool ref |
| pricing_model | jsonb | static / variable / dynamic (see protocol) |
| schema | jsonb | request/response schema for agents |
| sla | jsonb | uptime, latency claims |
| reputation | numeric | derived score |
| status | enum | `listed`, `unlisted` |

### `quotes`
A signed price challenge. Prevents replay.

| column | type | notes |
|--------|------|-------|
| id | uuid PK | |
| service_id | uuid FK | |
| agent_id | uuid FK | nullable until claimed |
| amount | bigint | minor units |
| currency | text | |
| nonce | text | unique |
| expires_at | timestamptz | TTL, typically seconds |
| signature | text | facilitator signature |

### `transactions`
The lifecycle record. **Idempotent on `request_hash`.**

| column | type | notes |
|--------|------|-------|
| id | uuid PK | |
| quote_id | uuid FK | |
| agent_id | uuid FK | |
| service_id | uuid FK | |
| amount | bigint | minor units |
| currency | text | |
| status | enum | `quoted → paid → settled → refunded / disputed / expired` |
| request_hash | text UNIQUE | idempotency key (hash of canonical request) |
| escrow | bool | true for agent-to-agent w/ delivery verification |
| created_at | timestamptz | |
| settled_at | timestamptz | nullable |

### `ledger_entries`
Append-only, double-entry. **The financial source of truth.**

| column | type | notes |
|--------|------|-------|
| id | bigserial PK | |
| transaction_id | uuid FK | |
| wallet_id | uuid FK | |
| direction | enum | `debit`, `credit` |
| amount | bigint | minor units, always positive |
| currency | text | |
| created_at | timestamptz | |

Invariant: **for every transaction, sum(debits) == sum(credits)** per currency.

### `receipts`
Signed proof for both parties (audit/tax/dispute).

| column | type | notes |
|--------|------|-------|
| id | uuid PK | |
| transaction_id | uuid FK | |
| party | enum | `buyer`, `seller` |
| payload | jsonb | canonical, signed |
| signature | text | |

### `meter_events`
Seller-side billable units. Idempotent per request.

| column | type | notes |
|--------|------|-------|
| id | bigserial PK | |
| service_id | uuid FK | |
| transaction_id | uuid FK | nullable |
| request_hash | text | dedupe key |
| units | numeric | complexity-weighted if variable pricing |
| created_at | timestamptz | |

## Core invariants

1. **No float money.** All amounts are `bigint` minor units + currency.
2. **Ledger is append-only.** Corrections are new compensating entries, never updates.
3. **Double-entry balances.** Every transaction's debits equal its credits per currency.
4. **Idempotency everywhere charges happen.** `transactions.request_hash` and
   `meter_events.request_hash` are unique; retries are no-ops, not double charges.
5. **Quotes expire.** A payment against an expired/consumed quote is rejected.
6. **Balance is derived.** Never store a mutable balance column; compute from the ledger
   (materialize a cached view for reads, reconciled against the ledger).
