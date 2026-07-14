package ledger

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is a Postgres-backed Store. It writes the transactions + ledger_entries
// tables in db/schema.sql, enforcing the same append-only, double-entry,
// idempotent contract as MemStore. Atomicity comes from a single DB transaction;
// idempotency from the UNIQUE constraint on transactions.request_hash.
type PGStore struct {
	pool *pgxpool.Pool
}

var _ Store = (*PGStore)(nil)

// NewPGStore connects to Postgres at dsn (e.g. postgres://user:pass@host:5432/db).
func NewPGStore(ctx context.Context, dsn string) (*PGStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &PGStore{pool: pool}, nil
}

// NewPGStoreFromPool wraps an existing pool (useful for sharing/config).
func NewPGStoreFromPool(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// Close releases the connection pool.
func (s *PGStore) Close() { s.pool.Close() }

const selectTxColumns = `id, quote_id, agent_id, service_id, amount, currency,
	status, request_hash, escrow, created_at, settled_at, rebate, rights`

// Post implements Store. Validation runs before any I/O; the idempotent insert
// and the entries write happen in one DB transaction.
func (s *PGStore) Post(ctx context.Context, p Posting) (Transaction, bool, error) {
	if err := Validate(p); err != nil {
		return Transaction{}, false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Transaction{}, false, err
	}
	defer tx.Rollback(ctx) // no-op once committed

	rights := p.Tx.Rights
	if rights == nil {
		rights = []string{} // NOT NULL text[]; no rights crossed
	}
	ct, err := tx.Exec(ctx, `
		INSERT INTO transactions
			(id, quote_id, agent_id, service_id, amount, currency, status, request_hash, escrow, created_at, settled_at, rebate, rights)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (request_hash) DO NOTHING`,
		p.Tx.ID, nullify(p.Tx.QuoteID), nullify(p.Tx.AgentID), nullify(p.Tx.ServiceID),
		p.Tx.Amount, p.Tx.Currency, string(p.Tx.Status), p.Tx.RequestHash, p.Tx.Escrow,
		p.Tx.CreatedAt, p.Tx.SettledAt, p.Tx.Rebate, rights)
	if err != nil {
		return Transaction{}, false, err
	}

	// Conflict on request_hash → a transaction already exists; write nothing and
	// return it. (A concurrent uncommitted insert of the same hash blocks here
	// until it commits/rolls back, so the read below is consistent.)
	if ct.RowsAffected() == 0 {
		existing, err := scanTx(tx.QueryRow(ctx,
			`SELECT `+selectTxColumns+` FROM transactions WHERE request_hash=$1`, p.Tx.RequestHash))
		if err != nil {
			return Transaction{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Transaction{}, false, err
		}
		return existing, false, nil
	}

	for _, e := range p.Entries {
		createdAt := e.CreatedAt
		if createdAt.IsZero() {
			createdAt = p.Tx.CreatedAt
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries
				(transaction_id, wallet_id, direction, amount, currency, created_at)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			p.Tx.ID, e.WalletID, string(e.Direction), e.Amount, e.Currency, createdAt); err != nil {
			return Transaction{}, false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Transaction{}, false, err
	}
	return p.Tx, true, nil
}

// Balance implements Store: sum(credits) - sum(debits) for a wallet+currency.
func (s *PGStore) Balance(ctx context.Context, walletID, currency string) (int64, error) {
	var bal int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN direction = 'credit' THEN amount ELSE -amount END), 0)
		FROM ledger_entries WHERE wallet_id = $1 AND currency = $2`,
		walletID, currency).Scan(&bal)
	if err != nil {
		return 0, err
	}
	return bal, nil
}

// TransactionByRequestHash implements Store.
func (s *PGStore) TransactionByRequestHash(ctx context.Context, requestHash string) (Transaction, bool, error) {
	t, err := scanTx(s.pool.QueryRow(ctx,
		`SELECT `+selectTxColumns+` FROM transactions WHERE request_hash=$1`, requestHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Transaction{}, false, nil
	}
	if err != nil {
		return Transaction{}, false, err
	}
	return t, true, nil
}

// Transactions implements Store. Filters are applied in SQL; results come back
// ordered by created_at (then id) so analytics reads are deterministic. The
// transactions_service_created index in db/schema.sql keeps the common
// service-scoped, time-bounded read cheap.
func (s *PGStore) Transactions(ctx context.Context, q TxQuery) ([]Transaction, error) {
	sql := `SELECT ` + selectTxColumns + ` FROM transactions WHERE 1=1`
	var args []any
	if q.ServiceID != "" {
		args = append(args, q.ServiceID)
		sql += ` AND service_id = $` + strconv.Itoa(len(args))
	}
	if !q.Since.IsZero() {
		args = append(args, q.Since.UTC())
		sql += ` AND created_at >= $` + strconv.Itoa(len(args))
	}
	if len(q.Statuses) > 0 {
		ss := make([]string, len(q.Statuses))
		for i, st := range q.Statuses {
			ss[i] = string(st)
		}
		args = append(args, ss)
		sql += ` AND status = ANY($` + strconv.Itoa(len(args)) + `)`
	}
	sql += ` ORDER BY created_at, id`

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Transaction
	for rows.Next() {
		tx, err := scanTx(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tx)
	}
	return out, rows.Err()
}

// scanTx reads a transaction row in selectTxColumns order.
func scanTx(row pgx.Row) (Transaction, error) {
	var (
		t                           Transaction
		quoteID, agentID, serviceID *string
		status                      string
		settledAt                   *time.Time
	)
	if err := row.Scan(&t.ID, &quoteID, &agentID, &serviceID, &t.Amount, &t.Currency,
		&status, &t.RequestHash, &t.Escrow, &t.CreatedAt, &settledAt,
		&t.Rebate, &t.Rights); err != nil {
		return Transaction{}, err
	}
	t.QuoteID = deref(quoteID)
	t.AgentID = deref(agentID)
	t.ServiceID = deref(serviceID)
	t.Status = Status(status)
	t.SettledAt = settledAt
	return t, nil
}

// nullify maps an empty string to a SQL NULL so nullable columns aren't filled
// with "".
func nullify(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
