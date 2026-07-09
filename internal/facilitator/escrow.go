package facilitator

import (
	"context"
	"fmt"
	"time"

	"github.com/tollgate/tollgate/internal/ledger"
)

// EscrowState is the lifecycle of an escrowed transaction.
type EscrowState string

const (
	EscrowHeld     EscrowState = "held"
	EscrowReleased EscrowState = "released"
	EscrowRefunded EscrowState = "refunded"
)

// EscrowRecord tracks funds held between a buyer and seller pending delivery
// confirmation (release) or a refund after the dispute window.
type EscrowRecord struct {
	TransactionID string
	BuyerWallet   string
	SellerWallet  string
	AgentID       string
	ServiceID     string
	Amount        int64
	Currency      string
	Status        EscrowState
	HeldAt        time.Time
	ReleaseAfter  time.Time // dispute window end (informational)
}

// EscrowResult is returned by Release/Refund.
type EscrowResult struct {
	TransactionID string
	Status        EscrowState
	ReceiptID     string
}

// Release moves escrowed funds to the seller on delivery confirmation. It is
// idempotent: a second release of an already-released escrow returns the same
// result and moves no funds.
func (c *Core) Release(ctx context.Context, txID string) (EscrowResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	rec, ok := c.escrows[txID]
	if !ok {
		return EscrowResult{}, fmt.Errorf("facilitator: no escrow for transaction %q", txID)
	}
	switch rec.Status {
	case EscrowReleased:
		return EscrowResult{TransactionID: txID, Status: EscrowReleased, ReceiptID: "rcpt-" + txID + "-seller"}, nil
	case EscrowRefunded:
		return EscrowResult{}, fmt.Errorf("facilitator: escrow %q already refunded", txID)
	}

	now := c.now().UTC()
	if err := c.moveEscrow(ctx, rec, rec.SellerWallet, "release:"+txID, now, ledger.StatusSettled); err != nil {
		return EscrowResult{}, err
	}
	rec.Status = EscrowReleased

	// Receipts issue on release — the point at which the seller is actually paid.
	c.issueReceipts(ctx, ledger.Transaction{
		ID: txID, AgentID: rec.AgentID, ServiceID: rec.ServiceID,
		Amount: rec.Amount, Currency: rec.Currency,
	})
	return EscrowResult{TransactionID: txID, Status: EscrowReleased, ReceiptID: "rcpt-" + txID + "-seller"}, nil
}

// Refund returns escrowed funds to the buyer (e.g. delivery not confirmed within
// the dispute window). Idempotent on an already-refunded escrow.
func (c *Core) Refund(ctx context.Context, txID string) (EscrowResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	rec, ok := c.escrows[txID]
	if !ok {
		return EscrowResult{}, fmt.Errorf("facilitator: no escrow for transaction %q", txID)
	}
	switch rec.Status {
	case EscrowRefunded:
		return EscrowResult{TransactionID: txID, Status: EscrowRefunded}, nil
	case EscrowReleased:
		return EscrowResult{}, fmt.Errorf("facilitator: escrow %q already released", txID)
	}

	now := c.now().UTC()
	if err := c.moveEscrow(ctx, rec, rec.BuyerWallet, "refund:"+txID, now, ledger.StatusRefunded); err != nil {
		return EscrowResult{}, err
	}
	rec.Status = EscrowRefunded
	return EscrowResult{TransactionID: txID, Status: EscrowRefunded}, nil
}

// Escrow returns the escrow record for a transaction, if any.
func (c *Core) Escrow(txID string) (EscrowRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.escrows[txID]
	if !ok {
		return EscrowRecord{}, false
	}
	return *rec, true
}

// moveEscrow posts the balanced entry that drains the escrow account to a
// destination wallet. The requestHash makes the ledger write idempotent.
func (c *Core) moveEscrow(ctx context.Context, rec *EscrowRecord, toWallet, requestHash string, now time.Time, status ledger.Status) error {
	settledAt := now
	_, _, err := c.store.Post(ctx, ledger.Posting{
		Tx: ledger.Transaction{
			ID: rec.TransactionID, AgentID: rec.AgentID, ServiceID: rec.ServiceID,
			Amount: rec.Amount, Currency: rec.Currency, Status: status,
			RequestHash: requestHash, Escrow: true, CreatedAt: now, SettledAt: &settledAt,
		},
		Entries: []ledger.Entry{
			{WalletID: walletEscrow, Direction: ledger.Debit, Amount: rec.Amount, Currency: rec.Currency, CreatedAt: now},
			{WalletID: toWallet, Direction: ledger.Credit, Amount: rec.Amount, Currency: rec.Currency, CreatedAt: now},
		},
	})
	return err
}
