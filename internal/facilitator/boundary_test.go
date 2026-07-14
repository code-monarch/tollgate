package facilitator_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/tollgate/tollgate/internal/facilitator"
	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/receipt"
	"github.com/tollgate/tollgate/internal/rights"
	"github.com/tollgate/tollgate/internal/settlement"
	"github.com/tollgate/tollgate/x402"
)

// boundary is a facilitator wired with one seller whose exhaust terms we control,
// and one funded buyer. It exercises the learning boundary end to end through the
// real quote → sign → settle path (docs/08-learning-boundary.md).
type boundary struct {
	core     *facilitator.Core
	store    ledger.Store
	signer   *x402.Signer
	agentKey ed25519.PrivateKey
}

const (
	bWallet = "wallet:buyer"
	sWallet = "wallet:seller"
	bAgent  = "agt_buyer"
	bSvc    = "svc_model"
)

func newBoundary(t *testing.T, offer rights.Offer) *boundary {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	signer, err := x402.NewSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	store := ledger.NewMemStore()
	core := facilitator.NewCore(store, signer, settlement.Mock{})
	core.RegisterService(facilitator.Service{
		ID: bSvc, SellerWallet: sWallet, Currency: "USDC",
		Network: "base", Asset: "USDC", PayTo: "0xSELLER",
		Exhaust: offer,
	})
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	core.RegisterAgent(facilitator.Agent{ID: bAgent, Wallet: bWallet, PublicKey: pub})
	if err := core.Fund(context.Background(), bAgent, "100000", "USDC"); err != nil {
		t.Fatal(err)
	}
	return &boundary{core: core, store: store, signer: signer, agentKey: priv}
}

// call runs one paid call, granting exactly the given rights.
func (b *boundary) call(t *testing.T, hash string, grant ...rights.Right) facilitator.SettleResult {
	t.Helper()
	ctx := context.Background()
	q, err := b.core.IssueQuote(ctx, facilitator.QuoteRequest{
		ServiceID: bSvc, Amount: "1000", Resource: "/infer",
	})
	if err != nil {
		t.Fatal(err)
	}
	p := x402.Payment{
		QuoteID: q.QuoteID, Nonce: q.Nonce, AgentID: bAgent, PayFrom: bWallet,
		Grant: rights.Strings(grant),
	}
	x402.SignPayment(b.agentKey, &p)
	res, err := b.core.Settle(ctx, q.QuoteID, p, hash, false)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (b *boundary) balance(t *testing.T, wallet string) int64 {
	t.Helper()
	bal, err := b.core.Balance(context.Background(), wallet, "USDC")
	if err != nil {
		t.Fatal(err)
	}
	return bal
}

// A seller that will not serve without training rights gets nothing when the buyer
// refuses: no funds move, and no data crosses. The hard boundary.
func TestBoundary_RequiredRightRefused_NothingCrosses(t *testing.T) {
	b := newBoundary(t, rights.Offer{Required: []rights.Right{rights.Train}})

	res := b.call(t, "req-1") // grant nothing

	if res.Settled {
		t.Fatal("settled despite the buyer refusing a required right — the boundary leaked")
	}
	if res.Reason == "" {
		t.Fatal("no reason given for the refusal")
	}
	if len(res.Rights) != 0 {
		t.Fatalf("rights crossed on a refused call: %v", res.Rights)
	}
	// Not a cent moved.
	if got := b.balance(t, bWallet); got != 100_000 {
		t.Fatalf("buyer balance = %d, want the full 100000 (nothing should have been charged)", got)
	}
	if got := b.balance(t, sWallet); got != 0 {
		t.Fatalf("seller was paid %d for a call it refused to serve", got)
	}
}

// The buyer refuses an *optional* right: the call proceeds at full list price and
// the receipt attests that nothing crossed.
func TestBoundary_OptionalRightRefused_PaysFullPriceNothingCrosses(t *testing.T) {
	b := newBoundary(t, rights.Offer{
		Optional: []rights.Right{rights.Train},
		Rebates:  map[rights.Right]int64{rights.Train: 300},
	})

	res := b.call(t, "req-1") // decline the ask

	if !res.Settled {
		t.Fatalf("call did not settle: %s", res.Reason)
	}
	if res.Rebate != 0 {
		t.Fatalf("rebate = %d, want 0 — a buyer who grants nothing earns nothing", res.Rebate)
	}
	if len(res.Rights) != 0 {
		t.Fatalf("rights = %v, want none to have crossed", res.Rights)
	}
	if got := b.balance(t, bWallet); got != 99_000 {
		t.Fatalf("buyer balance = %d, want 99000 (full list price, no discount)", got)
	}
	if got := b.balance(t, sWallet); got != 1000 {
		t.Fatalf("seller balance = %d, want the full 1000", got)
	}

	// The receipt is a positive attestation that nothing crossed.
	assertReceiptRights(t, b, res.TransactionID, nil, "0")
}

// The buyer grants training rights and is PAID for them: the seller's dividend is a
// real, separately-booked ledger leg, not an invisible discount.
func TestBoundary_GrantEarnsDividend_BookedGross(t *testing.T) {
	b := newBoundary(t, rights.Offer{
		Optional: []rights.Right{rights.Train, rights.Retain},
		Rebates:  map[rights.Right]int64{rights.Train: 300, rights.Retain: 50},
	})

	res := b.call(t, "req-1", rights.Train, rights.Retain)

	if !res.Settled {
		t.Fatalf("call did not settle: %s", res.Reason)
	}
	if res.Rebate != 350 {
		t.Fatalf("dividend = %d, want 350 (300 train + 50 retain)", res.Rebate)
	}
	// Net: buyer pays 1000 and earns back 350.
	if got := b.balance(t, bWallet); got != 99_350 {
		t.Fatalf("buyer balance = %d, want 99350 (paid 1000, earned 350 back)", got)
	}
	if got := b.balance(t, sWallet); got != 650 {
		t.Fatalf("seller balance = %d, want 650 (earned 1000, paid 350 for the knowledge)", got)
	}

	// Booked GROSS: the transaction records the full price AND the dividend, so a
	// seller's books show exactly what it paid to learn from this customer.
	txns, err := b.store.Transactions(context.Background(), ledger.TxQuery{ServiceID: bSvc})
	if err != nil {
		t.Fatal(err)
	}
	if len(txns) != 1 {
		t.Fatalf("want 1 transaction, got %d", len(txns))
	}
	if txns[0].Amount != 1000 || txns[0].Rebate != 350 {
		t.Fatalf("tx = amount %d / rebate %d, want 1000 / 350 booked gross",
			txns[0].Amount, txns[0].Rebate)
	}

	assertReceiptRights(t, b, res.TransactionID, []string{"retain", "train"}, "350")
}

// A buyer cannot be talked into granting more than the seller asked for, and a
// right never asked for earns nothing.
func TestBoundary_CannotGrantWhatWasNotAsked(t *testing.T) {
	b := newBoundary(t, rights.Offer{
		Optional: []rights.Right{rights.Retain},
		Rebates:  map[rights.Right]int64{rights.Retain: 50},
	})

	// The buyer over-grants: only `retain` was on the table.
	res := b.call(t, "req-1", rights.Retain, rights.Train, rights.Distill)

	if len(res.Rights) != 1 || res.Rights[0] != "retain" {
		t.Fatalf("effective rights = %v, want only [retain] — the unasked rights must be dropped", res.Rights)
	}
	if res.Rebate != 50 {
		t.Fatalf("dividend = %d, want 50", res.Rebate)
	}
}

// A generous dividend can make a call free, but never negative: the rail settles
// payments, not reverse-payments.
func TestBoundary_DividendCannotExceedPrice(t *testing.T) {
	b := newBoundary(t, rights.Offer{
		Optional: []rights.Right{rights.Train},
		Rebates:  map[rights.Right]int64{rights.Train: 5000}, // more than the 1000 price
	})

	res := b.call(t, "req-1", rights.Train)

	if res.Rebate != 1000 {
		t.Fatalf("dividend = %d, want it clamped to the 1000 price", res.Rebate)
	}
	if got := b.balance(t, bWallet); got != 100_000 {
		t.Fatalf("buyer balance = %d, want 100000 — the call is free, never negative", got)
	}
	if got := b.balance(t, sWallet); got != 0 {
		t.Fatalf("seller balance = %d, want 0 — it paid exactly what it earned", got)
	}
}

// Replaying a paid request re-grants nothing and re-pays no dividend.
func TestBoundary_IdempotentReplayDoesNotRegrant(t *testing.T) {
	b := newBoundary(t, rights.Offer{
		Optional: []rights.Right{rights.Train},
		Rebates:  map[rights.Right]int64{rights.Train: 300},
	})

	first := b.call(t, "same-hash", rights.Train)
	if !first.Fresh {
		t.Fatal("first call was not fresh")
	}
	afterFirst := b.balance(t, bWallet)

	// Same request hash → idempotent replay.
	ctx := context.Background()
	q, _ := b.core.IssueQuote(ctx, facilitator.QuoteRequest{ServiceID: bSvc, Amount: "1000", Resource: "/infer"})
	p := x402.Payment{QuoteID: q.QuoteID, Nonce: q.Nonce, AgentID: bAgent, PayFrom: bWallet,
		Grant: []string{"train"}}
	x402.SignPayment(b.agentKey, &p)
	replay, err := b.core.Settle(ctx, q.QuoteID, p, "same-hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Fresh {
		t.Fatal("replay reported fresh — it would double-pay the dividend")
	}
	if got := b.balance(t, bWallet); got != afterFirst {
		t.Fatalf("replay moved money: %d -> %d", afterFirst, got)
	}
}

// assertReceiptRights checks both parties' receipts carry the granted rights and
// the dividend, and that the signature covers them.
func assertReceiptRights(t *testing.T, b *boundary, txID string, wantRights []string, wantRebate string) {
	t.Helper()
	rs := b.core.Receipts(context.Background(), txID)
	if len(rs) != 2 {
		t.Fatalf("want a receipt for each party, got %d", len(rs))
	}
	pub := ed25519.PublicKey(b.core.ReceiptPublicKey())
	for _, r := range rs {
		if err := receipt.Verify(pub, r); err != nil {
			t.Fatalf("%s receipt failed verification: %v", r.Party, err)
		}
		if r.Rebate != wantRebate {
			t.Fatalf("%s receipt rebate = %q, want %q", r.Party, r.Rebate, wantRebate)
		}
		if len(r.Rights) != len(wantRights) {
			t.Fatalf("%s receipt rights = %v, want %v", r.Party, r.Rights, wantRights)
		}
		for i, want := range wantRights {
			if r.Rights[i] != want {
				t.Fatalf("%s receipt rights = %v, want %v", r.Party, r.Rights, wantRights)
			}
		}

		// The rights are covered by the signature: forging the record breaks it.
		forged := r
		forged.Rights = []string{"train", "distill", "share_third_party"}
		if err := receipt.Verify(pub, forged); err == nil {
			t.Fatal("a forged rights list still verified — the receipt does not bind the boundary")
		}
	}
}
