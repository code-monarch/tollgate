-- Tollgate ledger schema (Postgres). Source of truth for balances, disputes,
-- invoices and tax. Money is always integer minor units + currency code; the
-- ledger is append-only and balances are DERIVED, never stored mutably.
-- See docs/03-data-model.md.
--
-- IDs are prefixed, opaque strings (Stripe-style: agt_…, svc_…, q_…, txn_…,
-- rcpt_…, wallet:…) to match the public API surface in docs/06-api-spec.md and
-- docs/04-protocol.md. The Go ledger.Store interface maps 1:1 onto the
-- transactions + ledger_entries tables (internal/ledger/pgstore.go).

BEGIN;

CREATE TABLE IF NOT EXISTS agents (
    id            text PRIMARY KEY,
    owner_org_id  text NOT NULL,
    public_key    text NOT NULL,
    label         text,
    status        text NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'suspended', 'revoked')),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS wallets (
    id          text PRIMARY KEY,
    owner_type  text NOT NULL CHECK (owner_type IN ('agent', 'seller', 'org')),
    owner_id    text NOT NULL,
    currency    text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
-- Balances are derived from ledger_entries; there is deliberately no balance column.

CREATE TABLE IF NOT EXISTS policies (
    id          text PRIMARY KEY,
    scope_type  text NOT NULL CHECK (scope_type IN ('agent', 'org')),
    scope_id    text NOT NULL,
    version     int  NOT NULL,
    rules       jsonb NOT NULL,
    active      bool NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope_type, scope_id, version)
);
-- At most one active policy version per scope.
CREATE UNIQUE INDEX IF NOT EXISTS policies_one_active_per_scope
    ON policies (scope_type, scope_id) WHERE active;

CREATE TABLE IF NOT EXISTS services (
    id            text PRIMARY KEY,
    seller_id     text NOT NULL,
    name          text NOT NULL,
    endpoint      text NOT NULL,
    pricing_model jsonb NOT NULL,
    schema        jsonb,
    sla           jsonb,
    reputation    numeric NOT NULL DEFAULT 0,
    status        text NOT NULL DEFAULT 'listed'
                    CHECK (status IN ('listed', 'unlisted'))
);

CREATE TABLE IF NOT EXISTS quotes (
    id          text PRIMARY KEY,
    service_id  text NOT NULL,
    agent_id    text,
    amount      bigint NOT NULL CHECK (amount >= 0),  -- minor units
    currency    text NOT NULL,
    nonce       text NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    signature   text NOT NULL
);

CREATE TABLE IF NOT EXISTS transactions (
    id           text PRIMARY KEY,
    quote_id     text,
    agent_id     text,
    service_id   text,
    amount       bigint NOT NULL CHECK (amount >= 0),  -- minor units
    currency     text NOT NULL,
    status       text NOT NULL
                   CHECK (status IN ('quoted','paid','settled','refunded','disputed','expired')),
    request_hash text NOT NULL UNIQUE,                 -- idempotency key
    escrow       bool NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    settled_at   timestamptz,

    -- The learning boundary (docs/08-learning-boundary.md). rebate is the data
    -- dividend the seller paid back for the exhaust rights in `rights`; it is held
    -- gross (never netted off amount) so revenue and the cost of knowledge stay
    -- separately auditable. Net cost to the buyer is amount - rebate.
    rebate       bigint NOT NULL DEFAULT 0 CHECK (rebate >= 0 AND rebate <= amount),
    rights       text[] NOT NULL DEFAULT '{}'
);
-- Analytics reads the ledger directly (revenue/route, cohorts, elasticity), the
-- common shape being one service over a time window.
CREATE INDEX IF NOT EXISTS transactions_service_created ON transactions (service_id, created_at);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id             bigserial PRIMARY KEY,
    transaction_id text NOT NULL REFERENCES transactions(id),
    wallet_id      text NOT NULL,
    direction      text NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount         bigint NOT NULL CHECK (amount > 0),  -- always positive; direction carries sign
    currency       text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ledger_entries_wallet ON ledger_entries (wallet_id, currency);
CREATE INDEX IF NOT EXISTS ledger_entries_tx ON ledger_entries (transaction_id);

-- Invariant enforced by the writer (and checkable in tests/reconciliation):
--   for every transaction, sum of credits == sum of debits per currency.
-- Corrections are new compensating entries, never UPDATEs — the table is append-only.

CREATE TABLE IF NOT EXISTS receipts (
    id             text PRIMARY KEY,
    transaction_id text NOT NULL REFERENCES transactions(id),
    party          text NOT NULL CHECK (party IN ('buyer', 'seller')),
    payload        jsonb NOT NULL,
    signature      text NOT NULL
);

CREATE TABLE IF NOT EXISTS meter_events (
    id             bigserial PRIMARY KEY,
    service_id     text NOT NULL,
    transaction_id text,
    request_hash   text NOT NULL,
    units          numeric NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now()
);
-- Idempotent metering: one billable event per request.
CREATE UNIQUE INDEX IF NOT EXISTS meter_events_request ON meter_events (service_id, request_hash);

COMMIT;
