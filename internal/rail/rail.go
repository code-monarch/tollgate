// Package rail is the external stablecoin rail seam: sending stablecoin to an
// on-chain address. It is deliberately separate from per-call settlement.
//
// Per-call buyer->seller settlement in Tollgate is custodial and internal — the
// double-entry ledger IS the settlement, which is the only sane model for
// $0.001 micro-payments where on-chain gas would dwarf the payment. The real
// stablecoin rail (Bitnob) is used at the edges: paying a seller's accrued
// balance out to a stablecoin address (payout), and funding deposits. See
// docs/07-roadmap.md, Milestone 2.
package rail

import "context"

// Status is the lifecycle of an external transfer. Rails like Bitnob are async:
// a Send returns Pending and a webhook later moves it to Success or Failed.
type Status string

const (
	StatusPending Status = "pending"
	StatusSuccess Status = "success"
	StatusFailed  Status = "failed"
)

// Transfer is a request to send stablecoin to an address.
type Transfer struct {
	ToAddress   string // recipient on-chain address
	Amount      int64  // minor units (e.g. 2000000 == 2.00 USDT at 6 decimals)
	Currency    string // "USDC" | "USDT"
	Chain       string // "base", "ethereum", "polygon", ...
	Reference   string // caller-unique idempotency key
	Description string // optional memo
}

// Confirmation is the rail's acknowledgement of a Send.
type Confirmation struct {
	ProviderRef string // rail-side id (e.g. Bitnob transfer id)
	Status      Status
	FeeMinor    int64 // network/service fee the rail deducted, if reported
}

// Rail sends stablecoin to an address over an external provider.
type Rail interface {
	Send(ctx context.Context, t Transfer) (Confirmation, error)
}
