package facilitator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/money"
	"github.com/tollgate/tollgate/internal/rail"
)

// PayoutState is the lifecycle of a payout to an external stablecoin address.
type PayoutState string

const (
	PayoutPending PayoutState = "pending"
	PayoutSuccess PayoutState = "success"
	PayoutFailed  PayoutState = "failed"
)

// PayoutRecord tracks a withdrawal of a seller's internal balance to an on-chain
// address over the external rail.
type PayoutRecord struct {
	Reference     string
	TransactionID string
	SellerWallet  string
	ToAddress     string
	Chain         string
	Amount        int64
	Currency      string
	ProviderRef   string
	Status        PayoutState
	CreatedAt     time.Time
}

// PayoutRequest asks to pay a seller's internal balance out to an address.
type PayoutRequest struct {
	SellerWallet string
	ToAddress    string
	Chain        string
	Currency     string
	Amount       string // minor units
	Reference    string // idempotency key
}

// PayoutResult is the outcome of a payout attempt.
type PayoutResult struct {
	Reference     string
	TransactionID string
	ProviderRef   string
	Status        PayoutState
	Reason        string
}

// Payout debits a seller's internal balance and sends stablecoin to an address
// over the rail. Funds are reserved in the ledger first (debit seller, credit
// the external sink); if the rail rejects the send the reservation is reversed.
// The rail is asynchronous, so a Pending result is finalized later by a webhook
// (FinalizePayout). Idempotent on Reference.
func (c *Core) Payout(ctx context.Context, req PayoutRequest) (PayoutResult, error) {
	if req.Reference == "" {
		return PayoutResult{}, errors.New("payout: reference required")
	}
	if req.ToAddress == "" {
		return PayoutResult{}, errors.New("payout: to_address required")
	}
	amt, err := money.Parse(req.Amount, req.Currency)
	if err != nil {
		return PayoutResult{}, err
	}

	c.mu.Lock()
	if rec, ok := c.payouts[req.Reference]; ok {
		res := payoutResult(rec)
		c.mu.Unlock()
		return res, nil
	}
	bal, err := c.store.Balance(ctx, req.SellerWallet, req.Currency)
	if err != nil {
		c.mu.Unlock()
		return PayoutResult{}, err
	}
	if bal < amt.Minor {
		c.mu.Unlock()
		return PayoutResult{Reference: req.Reference, Status: PayoutFailed, Reason: "insufficient funds"}, nil
	}

	now := c.now().UTC()
	txID := newID("txn")
	if _, _, err := c.store.Post(ctx, ledger.Posting{
		Tx: ledger.Transaction{
			ID: txID, Amount: amt.Minor, Currency: req.Currency, Status: ledger.StatusPaid,
			RequestHash: "payout:" + req.Reference, CreatedAt: now,
		},
		Entries: []ledger.Entry{
			{WalletID: req.SellerWallet, Direction: ledger.Debit, Amount: amt.Minor, Currency: req.Currency, CreatedAt: now},
			{WalletID: walletExternal, Direction: ledger.Credit, Amount: amt.Minor, Currency: req.Currency, CreatedAt: now},
		},
	}); err != nil {
		c.mu.Unlock()
		return PayoutResult{}, err
	}
	rec := &PayoutRecord{
		Reference: req.Reference, TransactionID: txID, SellerWallet: req.SellerWallet,
		ToAddress: req.ToAddress, Chain: req.Chain, Amount: amt.Minor, Currency: req.Currency,
		Status: PayoutPending, CreatedAt: now,
	}
	c.payouts[req.Reference] = rec
	c.mu.Unlock()

	// Rail I/O happens without the lock held.
	conf, err := c.rail.Send(ctx, rail.Transfer{
		ToAddress: req.ToAddress, Amount: amt.Minor, Currency: req.Currency,
		Chain: req.Chain, Reference: req.Reference, Description: "tollgate payout",
	})

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.reversePayoutLocked(ctx, rec)
		rec.Status = PayoutFailed
		return PayoutResult{Reference: req.Reference, TransactionID: txID, Status: PayoutFailed, Reason: err.Error()}, nil
	}
	rec.ProviderRef = conf.ProviderRef
	switch conf.Status {
	case rail.StatusSuccess:
		rec.Status = PayoutSuccess
	case rail.StatusFailed:
		c.reversePayoutLocked(ctx, rec)
		rec.Status = PayoutFailed
	default:
		rec.Status = PayoutPending
	}
	return payoutResult(rec), nil
}

// FinalizePayout applies a rail webhook outcome to a pending payout. On success
// the reservation stands; on failure the seller is credited back. Idempotent:
// a payout already in a terminal state is unchanged.
func (c *Core) FinalizePayout(ctx context.Context, reference string, success bool) (PayoutResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	rec, ok := c.payouts[reference]
	if !ok {
		return PayoutResult{}, fmt.Errorf("facilitator: no payout for reference %q", reference)
	}
	if rec.Status == PayoutSuccess || rec.Status == PayoutFailed {
		return payoutResult(rec), nil
	}
	if success {
		rec.Status = PayoutSuccess
	} else {
		c.reversePayoutLocked(ctx, rec)
		rec.Status = PayoutFailed
	}
	return payoutResult(rec), nil
}

// Payout returns a payout record by reference.
func (c *Core) PayoutByReference(reference string) (PayoutRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.payouts[reference]
	if !ok {
		return PayoutRecord{}, false
	}
	return *rec, true
}

// reversePayoutLocked posts a compensating entry crediting the seller back when
// a payout fails. The requestHash makes the reversal idempotent. Caller holds c.mu.
func (c *Core) reversePayoutLocked(ctx context.Context, rec *PayoutRecord) {
	now := c.now().UTC()
	_, _, _ = c.store.Post(ctx, ledger.Posting{
		Tx: ledger.Transaction{
			ID: newID("txn"), Amount: rec.Amount, Currency: rec.Currency, Status: ledger.StatusRefunded,
			RequestHash: "payout-reverse:" + rec.Reference, CreatedAt: now,
		},
		Entries: []ledger.Entry{
			{WalletID: walletExternal, Direction: ledger.Debit, Amount: rec.Amount, Currency: rec.Currency, CreatedAt: now},
			{WalletID: rec.SellerWallet, Direction: ledger.Credit, Amount: rec.Amount, Currency: rec.Currency, CreatedAt: now},
		},
	})
}

func payoutResult(rec *PayoutRecord) PayoutResult {
	return PayoutResult{
		Reference:     rec.Reference,
		TransactionID: rec.TransactionID,
		ProviderRef:   rec.ProviderRef,
		Status:        rec.Status,
	}
}
