// Package receipt issues and verifies signed proofs of completed transactions.
// Each settled transaction yields one receipt per party (buyer and seller); both
// are signed by the facilitator key so either side — or an auditor — can verify
// a transaction happened, for dispute, tax and reconciliation
// (docs/03-data-model.md `receipts`, docs/07-roadmap.md Milestone 2).
package receipt

import (
	"context"
	"crypto/ed25519"
	"strings"
	"sync"
	"time"

	"github.com/tollgate/tollgate/x402"
)

// Party identifies which side a receipt belongs to.
type Party string

const (
	Buyer  Party = "buyer"
	Seller Party = "seller"
)

// Receipt is a signed proof of a completed transaction, held by one party.
type Receipt struct {
	ID            string    `json:"id"`
	TransactionID string    `json:"transactionId"`
	Party         Party     `json:"party"`
	AgentID       string    `json:"agentId"`
	ServiceID     string    `json:"serviceId"`
	Amount        string    `json:"amount"` // minor units
	Currency      string    `json:"currency"`
	IssuedAt      time.Time `json:"issuedAt"`
	Signature     string    `json:"signature"` // facilitator ed25519, base64
}

// signer is the subset of x402.Signer receipts need (also lets tests fake it).
type signer interface {
	SignMessage(msg []byte) string
}

// Issue builds and signs a receipt. id is caller-supplied (e.g. "rcpt-"+txID+
// "-buyer") so issuance is idempotent per (transaction, party).
func Issue(s signer, id string, r Receipt) Receipt {
	r.ID = id
	r.Signature = s.SignMessage(signingBytes(r))
	return r
}

// Verify checks a receipt's facilitator signature.
func Verify(pub ed25519.PublicKey, r Receipt) error {
	return x402.VerifyMessage(pub, signingBytes(r), r.Signature)
}

// signingBytes is the canonical, signature-free encoding of a receipt.
func signingBytes(r Receipt) []byte {
	var b strings.Builder
	fields := []string{
		r.ID, r.TransactionID, string(r.Party), r.AgentID, r.ServiceID,
		r.Amount, r.Currency, r.IssuedAt.UTC().Format(time.RFC3339Nano),
	}
	for i, f := range fields {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(f)
	}
	return []byte(b.String())
}

// Store persists receipts keyed by transaction id. In-memory for Milestone 2;
// db/schema.sql `receipts` is the eventual home.
type Store struct {
	mu   sync.Mutex
	byTx map[string][]Receipt
	byID map[string]Receipt
}

// NewStore returns an empty receipt store.
func NewStore() *Store {
	return &Store{byTx: make(map[string][]Receipt), byID: make(map[string]Receipt)}
}

// Put stores a receipt, idempotently on receipt id.
func (s *Store) Put(ctx context.Context, r Receipt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[r.ID]; ok {
		return
	}
	s.byID[r.ID] = r
	s.byTx[r.TransactionID] = append(s.byTx[r.TransactionID], r)
}

// ByTransaction returns both parties' receipts for a transaction.
func (s *Store) ByTransaction(ctx context.Context, txID string) []Receipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Receipt, len(s.byTx[txID]))
	copy(out, s.byTx[txID])
	return out
}
