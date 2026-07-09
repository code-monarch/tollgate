package ledger

import (
	"context"
	"sync"
)

// MemStore is an in-memory, concurrency-safe Store. It is the source of truth
// for the demo and tests; a Postgres store (db/schema.sql) is the next
// increment and satisfies the same interface.
type MemStore struct {
	mu      sync.Mutex
	seq     int64
	byHash  map[string]Transaction // idempotency index
	byID    map[string]Transaction
	entries []Entry
}

// NewMemStore returns an empty in-memory ledger.
func NewMemStore() *MemStore {
	return &MemStore{
		byHash: make(map[string]Transaction),
		byID:   make(map[string]Transaction),
	}
}

// Post implements Store. Validation runs before the lock; the idempotency check
// and append happen atomically under the lock.
func (m *MemStore) Post(ctx context.Context, p Posting) (Transaction, bool, error) {
	if err := Validate(p); err != nil {
		return Transaction{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.byHash[p.Tx.RequestHash]; ok {
		return existing, false, nil
	}

	for _, e := range p.Entries {
		m.seq++
		e.ID = m.seq
		e.TransactionID = p.Tx.ID
		if e.CreatedAt.IsZero() {
			e.CreatedAt = p.Tx.CreatedAt
		}
		m.entries = append(m.entries, e)
	}
	m.byHash[p.Tx.RequestHash] = p.Tx
	m.byID[p.Tx.ID] = p.Tx
	return p.Tx, true, nil
}

// Balance implements Store: sum(credits) - sum(debits) over the wallet+currency.
func (m *MemStore) Balance(ctx context.Context, walletID, currency string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var bal int64
	for _, e := range m.entries {
		if e.WalletID != walletID || e.Currency != currency {
			continue
		}
		if e.Direction == Credit {
			bal += e.Amount
		} else {
			bal -= e.Amount
		}
	}
	return bal, nil
}

// TransactionByRequestHash implements Store.
func (m *MemStore) TransactionByRequestHash(ctx context.Context, requestHash string) (Transaction, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.byHash[requestHash]
	return tx, ok, nil
}
