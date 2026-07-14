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
	"strings"
	"sync"
	"time"

	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/money"
	"github.com/tollgate/tollgate/internal/rail"
	"github.com/tollgate/tollgate/internal/receipt"
	"github.com/tollgate/tollgate/internal/rights"
	"github.com/tollgate/tollgate/internal/settlement"
	"github.com/tollgate/tollgate/x402"
)

// Internal ledger accounts. Balances across all accounts always net to zero, so
// these contra-accounts keep the double-entry invariant intact as money enters
// (treasury), is held mid-transaction (escrow), or leaves the platform (external).
const (
	walletTreasury = "wallet:treasury" // source of funded deposits
	walletEscrow   = "wallet:escrow"   // holds escrowed funds mid-transaction
	walletExternal = "wallet:external" // sink for funds paid out over a real rail
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

	// Exhaust is the seller's claim on the intelligence exhaust of a call: the
	// rights it requires, the rights it would like, and the dividend it will pay
	// for them. The zero value claims nothing — the safe default
	// (docs/08-learning-boundary.md).
	Exhaust rights.Offer
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
	rail     rail.Rail
	receipts *receipt.Store
	services map[string]Service
	agents   map[string]Agent
	quotes   map[string]x402.Quote
	nonces   map[string]bool          // nonce -> consumed
	escrows  map[string]*EscrowRecord // transaction id -> escrow
	payouts  map[string]*PayoutRecord // reference -> payout
	ttl      time.Duration
	dispute  time.Duration
	now      func() time.Time
}

// Option customizes a Core.
type Option func(*Core)

// WithTTL sets the quote time-to-live (default 30s).
func WithTTL(d time.Duration) Option { return func(c *Core) { c.ttl = d } }

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(c *Core) { c.now = now } }

// WithRail sets the external stablecoin rail used for payouts (default: mock).
func WithRail(r rail.Rail) Option { return func(c *Core) { c.rail = r } }

// WithDisputeWindow sets how long escrowed funds are eligible for refund before
// a delivery-confirmed release is expected (default 24h).
func WithDisputeWindow(d time.Duration) Option { return func(c *Core) { c.dispute = d } }

// NewCore builds a facilitator core over the given ledger, signer and settler.
// The external payout rail defaults to a mock; override with WithRail.
func NewCore(store ledger.Store, signer *x402.Signer, settler settlement.Settlement, opts ...Option) *Core {
	c := &Core{
		store:    store,
		signer:   signer,
		settler:  settler,
		rail:     rail.NewMock(),
		receipts: receipt.NewStore(),
		services: make(map[string]Service),
		agents:   make(map[string]Agent),
		quotes:   make(map[string]x402.Quote),
		nonces:   make(map[string]bool),
		escrows:  make(map[string]*EscrowRecord),
		payouts:  make(map[string]*PayoutRecord),
		ttl:      30 * time.Second,
		dispute:  24 * time.Hour,
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
			{WalletID: walletTreasury, Direction: ledger.Debit, Amount: amt.Minor, Currency: currency, CreatedAt: now},
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
		// The seller's rights ask travels inside the signed quote, so the buyer
		// decides on it with the same certainty it decides on the price.
		Exhaust: svc.Exhaust.ToWire(),
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

	// Rights are the exhaust rights that actually crossed the boundary on this
	// call, and Rebate is the data dividend the seller paid back for them. Both
	// are bound into the signed receipts (docs/08-learning-boundary.md).
	Rights []string
	Rebate int64
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

	// The learning boundary. The offer is read from the SIGNED quote — what the
	// buyer actually saw and consented against — not from the mutable registry.
	// The grant is covered by the buyer's own signature (verified above), so
	// neither side can rewrite what was asked or what was given.
	offer := rights.FromWire(q.Exhaust)
	granted := rights.ParseRights(p.Grant)

	// A seller that will not serve without rights the buyer refused gets nothing:
	// no funds move and no data crosses. This is the hard boundary.
	if missing := rights.Missing(offer, granted); len(missing) > 0 {
		return SettleResult{Reason: "required exhaust rights not granted: " +
			strings.Join(rights.Strings(missing), ", ")}, nil
	}

	// Only what was both asked for and granted crosses; the dividend is what the
	// seller pays back for it, and it can never exceed the price.
	effective := rights.Effective(offer, granted)
	rebate := rights.Clamp(rights.Rebate(offer, effective), amt.Minor)
	net := amt.Minor - rebate

	// Balance is derived from the ledger; insufficient funds is a distinct
	// rejection, not an error. The buyer only needs to cover the NET cost — the
	// dividend it earns offsets the price in the same atomic posting.
	bal, err := c.store.Balance(ctx, agent.Wallet, q.Currency)
	if err != nil {
		return SettleResult{}, err
	}
	if bal < net {
		return SettleResult{Reason: "insufficient funds"}, nil
	}

	now := c.now().UTC()
	txID := newID("txn")

	// Single-use nonce: consumed once verification passes and funds are
	// available, so a replay against a new request is rejected.
	c.nonces[q.Nonce] = true

	if escrow {
		// Agent-to-agent with delivery verification: hold funds by crediting the
		// escrow account. The transaction is "paid" (held), not "settled" —
		// Release or Refund finalizes it. Receipts issue on release, and so does
		// the data dividend: rights only vest against a completed call.
		written, _, err := c.store.Post(ctx, ledger.Posting{
			Tx: ledger.Transaction{
				ID: txID, QuoteID: quoteID, AgentID: agent.ID, ServiceID: svc.ID,
				Amount: amt.Minor, Currency: q.Currency, Status: ledger.StatusPaid,
				RequestHash: requestHash, Escrow: true, CreatedAt: now,
				Rebate: rebate, Rights: rights.Strings(effective),
			},
			Entries: []ledger.Entry{
				{WalletID: agent.Wallet, Direction: ledger.Debit, Amount: amt.Minor, Currency: q.Currency, CreatedAt: now},
				{WalletID: walletEscrow, Direction: ledger.Credit, Amount: amt.Minor, Currency: q.Currency, CreatedAt: now},
			},
		})
		if err != nil {
			return SettleResult{}, err
		}
		c.escrows[written.ID] = &EscrowRecord{
			TransactionID: written.ID, BuyerWallet: agent.Wallet, SellerWallet: svc.SellerWallet,
			AgentID: agent.ID, ServiceID: svc.ID, Amount: amt.Minor, Currency: q.Currency,
			Status: EscrowHeld, HeldAt: now, ReleaseAfter: now.Add(c.dispute),
			Rebate: rebate, Rights: rights.Strings(effective),
		}
		return SettleResult{
			TransactionID: written.ID,
			Status:        string(ledger.StatusPaid),
			Settled:       true,
			Fresh:         true,
			Rights:        rights.Strings(effective),
			Rebate:        rebate,
		}, nil
	}

	// Immediate settlement: move funds buyer -> seller. The internal custodial
	// move IS the settlement; the settler hook lets a rail observe it. The rail
	// observes the NET movement — the dividend is an internal counter-flow.
	//
	// A dividend generous enough to cover the whole price nets to zero: the call is
	// free, the buyer having paid for it entirely in knowledge. There is no funds
	// movement for the rail to observe, so we skip it — the ledger still records
	// both gross legs below, which is where the trade is actually visible.
	if net > 0 {
		if _, err := c.settler.Settle(ctx, settlement.Instruction{
			From: agent.Wallet, To: svc.SellerWallet, Amount: net,
			Currency: q.Currency, Ref: quoteID, Escrow: false,
		}); err != nil {
			return SettleResult{}, err
		}
	}
	settledAt := now
	written, _, err := c.store.Post(ctx, ledger.Posting{
		Tx: ledger.Transaction{
			ID: txID, QuoteID: quoteID, AgentID: agent.ID, ServiceID: svc.ID,
			Amount: amt.Minor, Currency: q.Currency, Status: ledger.StatusSettled,
			RequestHash: requestHash, CreatedAt: now, SettledAt: &settledAt,
			Rebate: rebate, Rights: rights.Strings(effective),
		},
		Entries: dividendEntries(agent.Wallet, svc.SellerWallet, amt.Minor, rebate, q.Currency, now),
	})
	if err != nil {
		return SettleResult{}, err
	}
	c.issueReceipts(ctx, written)

	return SettleResult{
		TransactionID: written.ID,
		Status:        string(written.Status),
		ReceiptID:     "rcpt-" + written.ID + "-seller",
		Settled:       true,
		Fresh:         true,
		Rights:        rights.Strings(effective),
		Rebate:        rebate,
	}, nil
}

// dividendEntries builds the balanced legs of a settlement. Without a dividend it
// is the ordinary two-leg move. With one it is four legs, gross: the buyer pays
// the full price and the seller pays back what the buyer's knowledge was worth.
//
//	debit  buyer   price     credit seller  price     (revenue)
//	debit  seller  rebate    credit buyer   rebate    (data dividend)
//
// Booking it gross rather than netting to a discount is the point: a seller's
// books then show exactly what it paid to learn from its customers, and a buyer's
// show exactly what its exhaust earned (docs/08-learning-boundary.md).
func dividendEntries(buyerWallet, sellerWallet string, price, rebate int64, currency string, at time.Time) []ledger.Entry {
	entries := []ledger.Entry{
		{WalletID: buyerWallet, Direction: ledger.Debit, Amount: price, Currency: currency, CreatedAt: at},
		{WalletID: sellerWallet, Direction: ledger.Credit, Amount: price, Currency: currency, CreatedAt: at},
	}
	if rebate > 0 {
		entries = append(entries,
			ledger.Entry{WalletID: sellerWallet, Direction: ledger.Debit, Amount: rebate, Currency: currency, CreatedAt: at},
			ledger.Entry{WalletID: buyerWallet, Direction: ledger.Credit, Amount: rebate, Currency: currency, CreatedAt: at},
		)
	}
	return entries
}

// newID returns a prefixed, random, URL-safe identifier.
func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("facilitator: crypto/rand failed: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
