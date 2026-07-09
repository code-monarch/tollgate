// Package settlement is the per-call, buyer->seller settlement seam. In
// Tollgate this is custodial and internal: the double-entry ledger IS the
// settlement (correct for $0.001 micro-payments, where on-chain gas would dwarf
// the payment). This interface is an observation/extension hook on that move;
// Mock is a no-op. External stablecoin movement (payouts, deposits) lives in
// internal/rail (see docs/07-roadmap.md, Milestone 2).
package settlement

import (
	"context"
	"fmt"
)

// Instruction describes a single settlement.
type Instruction struct {
	From     string // buyer wallet id
	To       string // seller wallet id
	Amount   int64  // positive minor units
	Currency string
	Ref      string // transaction/quote reference
	Escrow   bool   // hold in escrow until delivery confirmation
}

// Receipt is the rail's acknowledgement of a settlement.
type Receipt struct {
	Ref     string // rail reference (e.g. tx hash)
	Settled bool
}

// Settlement is the swappable rail interface. Nothing else in Tollgate knows
// whether funds move on-chain or in a mock.
type Settlement interface {
	Settle(ctx context.Context, in Instruction) (Receipt, error)
}

// Mock settles instantly and deterministically, standing in for a stablecoin
// rail. It performs no I/O.
type Mock struct{}

// Settle implements Settlement.
func (Mock) Settle(ctx context.Context, in Instruction) (Receipt, error) {
	if in.Amount <= 0 {
		return Receipt{}, fmt.Errorf("settlement: non-positive amount %d", in.Amount)
	}
	if in.From == "" || in.To == "" {
		return Receipt{}, fmt.Errorf("settlement: missing wallet (from=%q to=%q)", in.From, in.To)
	}
	return Receipt{Ref: "mock-settle:" + in.Ref, Settled: true}, nil
}
