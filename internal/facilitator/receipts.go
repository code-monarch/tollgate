package facilitator

import (
	"context"
	"strconv"

	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/receipt"
)

// issueReceipts signs and stores a buyer and a seller receipt for a settled
// transaction. Receipt ids are deterministic per (transaction, party) so the
// store dedupes if issuance is retried. The caller holds c.mu.
//
// The granted exhaust rights and the dividend paid for them are bound into both
// receipts, so each side leaves the call holding signed proof of where the
// learning boundary was drawn (docs/08-learning-boundary.md).
func (c *Core) issueReceipts(ctx context.Context, tx ledger.Transaction) {
	amount := strconv.FormatInt(tx.Amount, 10)
	rebate := strconv.FormatInt(tx.Rebate, 10)
	issuedAt := c.now().UTC()
	for _, party := range []receipt.Party{receipt.Buyer, receipt.Seller} {
		r := receipt.Issue(c.signer, "rcpt-"+tx.ID+"-"+string(party), receipt.Receipt{
			TransactionID: tx.ID,
			Party:         party,
			AgentID:       tx.AgentID,
			ServiceID:     tx.ServiceID,
			Amount:        amount,
			Currency:      tx.Currency,
			IssuedAt:      issuedAt,
			Rights:        tx.Rights,
			Rebate:        rebate,
		})
		c.receipts.Put(ctx, r)
	}
}

// Receipts returns both parties' receipts for a transaction.
func (c *Core) Receipts(ctx context.Context, txID string) []receipt.Receipt {
	return c.receipts.ByTransaction(ctx, txID)
}

// ReceiptPublicKey exposes the facilitator key receipts are verified against.
func (c *Core) ReceiptPublicKey() []byte { return c.signer.PublicKey() }
