// Package facilitator is the rail: it issues signed quotes, verifies payment
// proofs, and settles funds while writing the double-entry ledger. The two hard,
// defensible steps of the whole platform — verify and settle+meter — live here
// (docs/02-architecture.md, docs/04-protocol.md).
package facilitator

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/money"
	"github.com/tollgate/tollgate/internal/settlement"
	"github.com/tollgate/tollgate/x402"
)

// Service is a registered sellable endpoint. Milestone 1 keeps the registry
// in-memory; docs/03-data-model.md `services` is the eventual home.
type Service struct {
	ID           string
	SellerWallet string // ledger wallet credited on settle
	Currency     string
	Network      string
	Asset        string
	PayTo        string // on-chain address (display only)
}

// Agent is a buyer identity bound to a wallet and an ed25519 verifying key.
type Agent struct {
	ID        string
	Wallet    string
	PublicKey ed25519.PublicKey
}

// Core is the facilitator's business logic, independent of HTTP.
type Core struct {
	mu       sync.Mutex
	store    ledger.Store
	signer   *x402.Signer
	settler  settlement.Settlement
	services map[string]Service
	agents   map[string]Agent
	quotes   map[string]x402.Quote
	nonces   map[string]bool // nonce -> consumed
	ttl      time.Duration
	now      func() time.Time
}

// Option customizes a Core.
type Option func(*Core)

// WithTTL sets the quote time-to-live (default 30s).
func WithTTL(d time.Duration) Option { return func(c *Core) { c.ttl = d } }

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(c *Core) { c.now = now } }

// NewCore builds a facilitator core over the given ledger, signer and rail.
func NewCore(store ledger.Store, signer *x402.Signer, settler settlement.Settlement, opts ...Option) *Core {
	c := &Core{
		store:    store,
		signer:   signer,
		settler:  settler,
		services: make(map[string]Service),
		agents:   make(map[string]Agent),
		quotes:   make(map[string]x402.Quote),
		nonces:   make(map[string]bool),
		ttl:      30 * time.Second,
		now:      time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// RegisterService adds/replaces a service in the registry.
func (c *Core) RegisterService(s Service) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.services[s.ID] = s
}

// RegisterAgent adds/replaces an agent in the registry.
func (c *Core) RegisterAgent(a Agent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agents[a.ID] = a
}

// Balance returns a wallet's derived balance from the ledger.
func (c *Core) Balance(ctx context.Context, walletID, currency string) (int64, error) {
	return c.store.Balance(ctx, walletID, currency)
}

// Fund credits an agent's wallet as a balanced deposit (debit treasury, credit
// agent). Stands in for docs/06-api-spec.md wallet/fund in Milestone 1.
func (c *Core) Fund(ctx context.Context, agentID, amount, currency string) error {
	c.mu.Lock()
	agent, ok := c.agents[agentID]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("facilitator: unknown agent %q", agentID)
	}
	amt, err := money.Parse(amount, currency)
	if err != nil {
		return err
	}
	now := c.now().UTC()
	id := newID("txn")
	_, _, err = c.store.Post(ctx, ledger.Posting{
		Tx: ledger.Transaction{
			ID: id, AgentID: agentID, Amount: amt.Minor, Currency: currency,
			Status: ledger.StatusSettled, RequestHash: "fund:" + id,
			CreatedAt: now, SettledAt: &now,
		},
		Entries: []ledger.Entry{
			{WalletID: "wallet:treasury", Direction: ledger.Debit, Amount: amt.Minor, Currency: currency, CreatedAt: now},
			{WalletID: agent.Wallet, Direction: ledger.Credit, Amount: amt.Minor, Currency: currency, CreatedAt: now},
		},
	})
	return err
}

// QuoteRequest is the input to IssueQuote.
type QuoteRequest struct {
	ServiceID string
	Amount    string // minor units
	Resource  string
}

// IssueQuote prices a resource and returns a signed, time-boxed quote.
func (c *Core) IssueQuote(ctx context.Context, req QuoteRequest) (x402.Quote, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	svc, ok := c.services[req.ServiceID]
	if !ok {
		return x402.Quote{}, fmt.Errorf("facilitator: unknown service %q", req.ServiceID)
	}
	amt, err := money.Parse(req.Amount, svc.Currency)
	if err != nil {
		return x402.Quote{}, err
	}
	q := x402.Quote{
		QuoteID:   newID("q"),
		ServiceID: svc.ID,
		Scheme:    "exact",
		Network:   svc.Network,
		Asset:     svc.Asset,
		Amount:    amt.String(),
		Currency:  svc.Currency,
		PayTo:     svc.PayTo,
		Resource:  req.Resource,
		Nonce:     newID("n"),
		ExpiresAt: c.now().Add(c.ttl).UTC(),
	}
	c.signer.SignQuote(&q)
	c.quotes[q.QuoteID] = q
	c.nonces[q.Nonce] = false
	return q, nil
}

// VerifyResult is the outcome of verifying a payment proof. A failed proof is
// Valid=false with a Reason — not a Go error.
type VerifyResult struct {
	Valid   bool
	AgentID string
	Amount  string
	Reason  string
}

// Verify validates a payment proof against a quote without settling.
func (c *Core) Verify(ctx context.Context, quoteID string, p x402.Payment) (VerifyResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.verifyLocked(quoteID, p), nil
}

// verifyLocked runs the verification checks; the caller must hold c.mu.
func (c *Core) verifyLocked(quoteID string, p x402.Payment) VerifyResult {
	q, ok := c.quotes[quoteID]
	if !ok {
		return VerifyResult{Reason: "unknown quote"}
	}
	if err := x402.VerifyQuote(c.signer.PublicKey(), q); err != nil {
		return VerifyResult{Reason: "bad quote signature"}
	}
	if c.now().After(q.ExpiresAt) {
		return VerifyResult{Reason: "quote expired"}
	}
	consumed, known := c.nonces[q.Nonce]
	if !known {
		return VerifyResult{Reason: "unknown nonce"}
	}
	if consumed {
		return VerifyResult{Reason: "nonce already used"}
	}
	if p.QuoteID != q.QuoteID || p.Nonce != q.Nonce {
		return VerifyResult{Reason: "payment does not match quote"}
	}
	agent, ok := c.agents[p.AgentID]
	if !ok {
		return VerifyResult{Reason: "unknown agent"}
	}
	if p.PayFrom != agent.Wallet {
		return VerifyResult{Reason: "payment wallet mismatch"}
	}
	if err := x402.VerifyPayment(agent.PublicKey, p); err != nil {
		return VerifyResult{Reason: "invalid payment signature"}
	}
	return VerifyResult{Valid: true, AgentID: agent.ID, Amount: q.Amount}
}

// SettleResult is the outcome of a settle attempt. Fresh is true only when this
// call actually moved funds — false for an idempotent replay — so callers meter
// exactly once per real charge.
type SettleResult struct {
	TransactionID string
	Status        string
	ReceiptID     string
	Reason        string
	Settled       bool
	Fresh         bool
}

// Settle verifies a proof then moves funds and writes the ledger pair. It is
// idempotent on requestHash: a retry of the same request returns the original
// transaction and charges nothing.
func (c *Core) Settle(ctx context.Context, quoteID string, p x402.Payment, requestHash string, escrow bool) (SettleResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Idempotency first: an existing tx for this request hash is a no-op replay.
	if tx, ok, err := c.store.TransactionByRequestHash(ctx, requestHash); err != nil {
		return SettleResult{}, err
	} else if ok {
		return SettleResult{
			TransactionID: tx.ID,
			Status:        string(tx.Status),
			ReceiptID:     "rcpt-" + tx.ID,
			Settled:       tx.Status == ledger.StatusSettled,
			Fresh:         false,
		}, nil
	}

	vr := c.verifyLocked(quoteID, p)
	if !vr.Valid {
		return SettleResult{Reason: vr.Reason}, nil
	}

	q := c.quotes[quoteID]
	svc := c.services[q.ServiceID]
	agent := c.agents[vr.AgentID]
	amt, err := money.Parse(q.Amount, q.Currency)
	if err != nil {
		return SettleResult{}, err
	}

	// Balance is derived from the ledger; insufficient funds is a distinct
	// rejection, not an error.
	bal, err := c.store.Balance(ctx, agent.Wallet, q.Currency)
	if err != nil {
		return SettleResult{}, err
	}
	if bal < amt.Minor {
		return SettleResult{Reason: "insufficient funds"}, nil
	}

	// Move funds over the rail (mock in Milestone 1).
	if _, err := c.settler.Settle(ctx, settlement.Instruction{
		From: agent.Wallet, To: svc.SellerWallet, Amount: amt.Minor,
		Currency: q.Currency, Ref: quoteID, Escrow: escrow,
	}); err != nil {
		return SettleResult{}, err
	}

	now := c.now().UTC()
	txID := newID("txn")
	settledAt := now
	posting := ledger.Posting{
		Tx: ledger.Transaction{
			ID: txID, QuoteID: quoteID, AgentID: agent.ID, ServiceID: svc.ID,
			Amount: amt.Minor, Currency: q.Currency, Status: ledger.StatusSettled,
			RequestHash: requestHash, Escrow: escrow, CreatedAt: now, SettledAt: &settledAt,
		},
		Entries: []ledger.Entry{
			{WalletID: agent.Wallet, Direction: ledger.Debit, Amount: amt.Minor, Currency: q.Currency, CreatedAt: now},
			{WalletID: svc.SellerWallet, Direction: ledger.Credit, Amount: amt.Minor, Currency: q.Currency, CreatedAt: now},
		},
	}
	written, _, err := c.store.Post(ctx, posting)
	if err != nil {
		return SettleResult{}, err
	}

	// Single-use nonce: consumed only after a successful settle, so a failed
	// attempt can be retried but a replay against a new request is rejected.
	c.nonces[q.Nonce] = true

	return SettleResult{
		TransactionID: written.ID,
		Status:        string(written.Status),
		ReceiptID:     "rcpt-" + written.ID,
		Settled:       true,
		Fresh:         true,
	}, nil
}

// newID returns a prefixed, random, URL-safe identifier.
func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("facilitator: crypto/rand failed: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
