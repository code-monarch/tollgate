package facilitator

import (
	"context"

	tollgate "github.com/tollgate/tollgate/sdk/go"
	"github.com/tollgate/tollgate/x402"
)

// InProcess adapts a Core to the seller SDK's tollgate.Facilitator interface so
// the Guard middleware can call the facilitator directly, with no HTTP hop —
// used by the demo and tests.
type InProcess struct{ core *Core }

// AsFacilitator returns an in-process facilitator client backed by c.
func (c *Core) AsFacilitator() tollgate.Facilitator { return &InProcess{core: c} }

// Quote implements tollgate.Facilitator.
func (a *InProcess) Quote(ctx context.Context, req tollgate.QuoteRequest) (x402.Quote, error) {
	return a.core.IssueQuote(ctx, QuoteRequest{
		ServiceID: req.ServiceID, Amount: req.Amount, Resource: req.Resource,
	})
}

// Settle implements tollgate.Facilitator.
func (a *InProcess) Settle(ctx context.Context, req tollgate.SettleRequest) (tollgate.SettleResponse, error) {
	p, err := x402.DecodePayment(req.Proof)
	if err != nil {
		return tollgate.SettleResponse{}, err
	}
	res, err := a.core.Settle(ctx, req.QuoteID, p, req.RequestHash, req.Escrow)
	if err != nil {
		return tollgate.SettleResponse{}, err
	}
	return tollgate.SettleResponse{
		TransactionID: res.TransactionID,
		Status:        res.Status,
		ReceiptID:     res.ReceiptID,
		Reason:        res.Reason,
		Settled:       res.Settled,
		Fresh:         res.Fresh,
	}, nil
}
