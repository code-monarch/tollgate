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
//
// It is also the instrument the Reverse Information Paradox needs: by binding the
// exhaust rights that crossed the boundary into the same signed artifact as the
// payment, it becomes a non-repudiable record of exactly what was disclosed, what
// was granted, what was refused, and what was paid for it. If a seller later trains
// on a trace it was never granted, the buyer holds the proof — and an honest seller
// holds proof that it did not (docs/08-learning-boundary.md).
type Receipt struct {
	ID            string    `json:"id"`
	TransactionID string    `json:"transactionId"`
	Party         Party     `json:"party"`
	AgentID       string    `json:"agentId"`
	ServiceID     string    `json:"serviceId"`
	Amount        string    `json:"amount"` // gross price, minor units
	Currency      string    `json:"currency"`
	IssuedAt      time.Time `json:"issuedAt"`

	// Rights are the exhaust rights granted on this call, sorted. An empty list is
	// a positive claim, not an absence: it attests that nothing crossed.
	Rights []string `json:"rights"`
	// Rebate is the data dividend the seller paid for those rights, minor units.
	Rebate string `json:"rebate"`

	Signature string `json:"signature"` // facilitator ed25519, base64
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

// signingBytes is the canonical, signature-free encoding of a receipt. The granted
// rights and the dividend are covered, so neither party can later alter the record
// of what crossed the boundary. Rights are sorted for a stable encoding; a rebate
// of "" normalizes to "0".
func signingBytes(r Receipt) []byte {
	rebate := r.Rebate
	if rebate == "" {
		rebate = "0"
	}
	var b strings.Builder
	fields := []string{
		r.ID, r.TransactionID, string(r.Party), r.AgentID, r.ServiceID,
		r.Amount, r.Currency, r.IssuedAt.UTC().Format(time.RFC3339Nano),
		strings.Join(x402.SortedRights(r.Rights), ","), rebate,
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
