// Package ledger is the append-only, double-entry source of truth for Tollgate.
// Balances are DERIVED from entries, never stored mutably. Writes are idempotent
// on a request hash so retries are no-ops, not double charges
// (docs/03-data-model.md, core invariants).
package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Direction is the sign of a ledger entry.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// Status is the transaction lifecycle state.
type Status string

const (
	StatusQuoted   Status = "quoted"
	StatusPaid     Status = "paid"
	StatusSettled  Status = "settled"
	StatusRefunded Status = "refunded"
	StatusDisputed Status = "disputed"
	StatusExpired  Status = "expired"
)

// Entry is one leg of a double-entry posting. Amount is always positive; the
// Direction carries the sign.
type Entry struct {
	ID            int64
	TransactionID string
	WalletID      string
	Direction     Direction
	Amount        int64 // positive minor units
	Currency      string
	CreatedAt     time.Time
}

// Transaction is the lifecycle record. It is idempotent on RequestHash.
type Transaction struct {
	ID          string
	QuoteID     string
	AgentID     string
	ServiceID   string
	Amount      int64
	Currency    string
	Status      Status
	RequestHash string
	Escrow      bool
	CreatedAt   time.Time
	SettledAt   *time.Time
}

// Posting is a transaction plus its balanced entries, written atomically.
type Posting struct {
	Tx      Transaction
	Entries []Entry
}

// ErrUnbalanced is returned when a posting's debits and credits do not net to
// zero per currency.
var ErrUnbalanced = errors.New("ledger: debits != credits")

// Store is the ledger persistence contract. A Postgres implementation maps 1:1
// onto db/schema.sql; MemStore is the in-process implementation used by the
// demo and tests.
type Store interface {
	// Post writes tx+entries atomically. It is idempotent on tx.RequestHash: if
	// a transaction with that hash already exists, it returns that transaction
	// with created=false and writes nothing.
	Post(ctx context.Context, p Posting) (tx Transaction, created bool, err error)
	// Balance is sum(credits) - sum(debits) for a wallet+currency.
	Balance(ctx context.Context, walletID, currency string) (int64, error)
	// TransactionByRequestHash returns the existing transaction, or ok=false.
	TransactionByRequestHash(ctx context.Context, requestHash string) (Transaction, bool, error)
}

// Validate enforces the invariants a posting must satisfy before it is written:
// a request hash, at least one entry, positive entry amounts, valid directions,
// and debits == credits per currency.
func Validate(p Posting) error {
	if p.Tx.RequestHash == "" {
		return errors.New("ledger: empty request hash")
	}
	if len(p.Entries) == 0 {
		return errors.New("ledger: posting has no entries")
	}
	net := map[string]int64{}
	for _, e := range p.Entries {
		if e.Amount <= 0 {
			return fmt.Errorf("ledger: non-positive entry amount %d", e.Amount)
		}
		switch e.Direction {
		case Debit:
			net[e.Currency] -= e.Amount
		case Credit:
			net[e.Currency] += e.Amount
		default:
			return fmt.Errorf("ledger: invalid direction %q", e.Direction)
		}
	}
	for cur, n := range net {
		if n != 0 {
			return fmt.Errorf("%w: currency %s off by %d", ErrUnbalanced, cur, n)
		}
	}
	return nil
}
