package facilitator_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/tollgate/tollgate/internal/facilitator"
	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/rail"
	"github.com/tollgate/tollgate/internal/receipt"
	"github.com/tollgate/tollgate/internal/settlement"
	"github.com/tollgate/tollgate/x402"
)

// coreHarness drives the facilitator Core directly (no HTTP) for Milestone 2
// features. It reuses the constants declared in e2e_test.go (same package).
type coreHarness struct {
	core     *facilitator.Core
	signer   *x402.Signer
	agentKey ed25519.PrivateKey
	n        int
}

func newCoreHarness(t *testing.T, opts ...facilitator.Option) *coreHarness {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	signer, err := x402.NewSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	core := facilitator.NewCore(ledger.NewMemStore(), signer, settlement.Mock{}, opts...)
	core.RegisterService(facilitator.Service{
		ID: serviceID, SellerWallet: sellerWallet, Currency: "USDC",
		Network: "base", Asset: "USDC", PayTo: "0xSELLER",
	})
	agentPub, agentPriv, _ := ed25519.GenerateKey(rand.Reader)
	core.RegisterAgent(facilitator.Agent{ID: agentID, Wallet: agentWallet, PublicKey: agentPub})
	if err := core.Fund(context.Background(), agentID, "1000000", "USDC"); err != nil {
		t.Fatal(err)
	}
	return &coreHarness{core: core, signer: signer, agentKey: agentPriv}
}

// settle issues a fresh quote and settles it, escrowed or not. Each call uses a
// unique request path so request hashes differ.
func (h *coreHarness) settle(t *testing.T, escrow bool) facilitator.SettleResult {
	t.Helper()
	h.n++
	q, err := h.core.IssueQuote(context.Background(), facilitator.QuoteRequest{
		ServiceID: serviceID, Amount: price, Resource: "https://x/geocode",
	})
	if err != nil {
		t.Fatal(err)
	}
	p := x402.Payment{QuoteID: q.QuoteID, Nonce: q.Nonce, AgentID: agentID, PayFrom: agentWallet}
	x402.SignPayment(h.agentKey, &p)
	hash := x402.RequestHash("GET", "/geocode", "n="+string(rune('a'+h.n)), q.QuoteID)
	res, err := h.core.Settle(context.Background(), q.QuoteID, p, hash, escrow)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Settled {
		t.Fatalf("settle rejected: %s", res.Reason)
	}
	return res
}

func (h *coreHarness) bal(t *testing.T, wallet string) int64 {
	t.Helper()
	b, err := h.core.Balance(context.Background(), wallet, "USDC")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEscrow_HoldThenRelease(t *testing.T) {
	h := newCoreHarness(t)
	res := h.settle(t, true)

	// Held: buyer debited, funds in escrow, seller not yet paid, status paid.
	if res.Status != string(ledger.StatusPaid) {
		t.Fatalf("escrow settle status = %s, want paid", res.Status)
	}
	if h.bal(t, agentWallet) != 999_000 {
		t.Fatalf("agent = %d, want 999000", h.bal(t, agentWallet))
	}
	if h.bal(t, "wallet:escrow") != 1000 {
		t.Fatalf("escrow = %d, want 1000", h.bal(t, "wallet:escrow"))
	}
	if h.bal(t, sellerWallet) != 0 {
		t.Fatalf("seller = %d, want 0 before release", h.bal(t, sellerWallet))
	}

	// Release → seller paid, escrow drained, receipts issued & verifiable.
	rel, err := h.core.Release(context.Background(), res.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Status != facilitator.EscrowReleased {
		t.Fatalf("release status = %s", rel.Status)
	}
	if h.bal(t, sellerWallet) != 1000 || h.bal(t, "wallet:escrow") != 0 {
		t.Fatalf("after release: seller=%d escrow=%d", h.bal(t, sellerWallet), h.bal(t, "wallet:escrow"))
	}

	receipts := h.core.Receipts(context.Background(), res.TransactionID)
	if len(receipts) != 2 {
		t.Fatalf("receipts = %d, want 2", len(receipts))
	}
	pub := ed25519.PublicKey(h.core.ReceiptPublicKey())
	for _, r := range receipts {
		if err := receipt.Verify(pub, r); err != nil {
			t.Fatalf("receipt %s failed verify: %v", r.ID, err)
		}
	}
}

func TestEscrow_Refund(t *testing.T) {
	h := newCoreHarness(t)
	res := h.settle(t, true)

	if _, err := h.core.Refund(context.Background(), res.TransactionID); err != nil {
		t.Fatal(err)
	}
	// Buyer made whole; escrow drained; seller never paid.
	if h.bal(t, agentWallet) != 1_000_000 {
		t.Fatalf("agent after refund = %d, want 1000000", h.bal(t, agentWallet))
	}
	if h.bal(t, "wallet:escrow") != 0 || h.bal(t, sellerWallet) != 0 {
		t.Fatalf("escrow=%d seller=%d", h.bal(t, "wallet:escrow"), h.bal(t, sellerWallet))
	}
	// Releasing a refunded escrow is an error.
	if _, err := h.core.Release(context.Background(), res.TransactionID); err == nil {
		t.Fatal("release after refund should fail")
	}
}

func TestEscrow_ReleaseIdempotent(t *testing.T) {
	h := newCoreHarness(t)
	res := h.settle(t, true)

	if _, err := h.core.Release(context.Background(), res.TransactionID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.core.Release(context.Background(), res.TransactionID); err != nil {
		t.Fatalf("second release should be a no-op, got %v", err)
	}
	if h.bal(t, sellerWallet) != 1000 {
		t.Fatalf("double release paid seller %d, want 1000", h.bal(t, sellerWallet))
	}
}

func TestReceipts_OnImmediateSettle(t *testing.T) {
	h := newCoreHarness(t)
	res := h.settle(t, false)

	receipts := h.core.Receipts(context.Background(), res.TransactionID)
	if len(receipts) != 2 {
		t.Fatalf("receipts = %d, want 2", len(receipts))
	}
	var parties []string
	pub := ed25519.PublicKey(h.core.ReceiptPublicKey())
	for _, r := range receipts {
		if err := receipt.Verify(pub, r); err != nil {
			t.Fatalf("verify %s: %v", r.ID, err)
		}
		if r.Amount != price {
			t.Fatalf("receipt amount = %s, want %s", r.Amount, price)
		}
		parties = append(parties, string(r.Party))
	}
	// Tampering must break verification.
	bad := receipts[0]
	bad.Amount = "999999"
	if err := receipt.Verify(pub, bad); err == nil {
		t.Fatal("tampered receipt verified")
	}
}

// failingRail returns an error on Send, to exercise the payout reversal path.
type failingRail struct{}

func (failingRail) Send(context.Context, rail.Transfer) (rail.Confirmation, error) {
	return rail.Confirmation{}, errors.New("rail down")
}

func TestPayout_SuccessDebitsSeller(t *testing.T) {
	mock := rail.NewMock()
	h := newCoreHarness(t, facilitator.WithRail(mock))
	h.settle(t, false) // seller now holds 1000

	res, err := h.core.Payout(context.Background(), facilitator.PayoutRequest{
		SellerWallet: sellerWallet, ToAddress: "0xSELLERADDR", Chain: "base",
		Currency: "USDC", Amount: "1000", Reference: "po-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != facilitator.PayoutSuccess {
		t.Fatalf("payout status = %s, want success", res.Status)
	}
	if h.bal(t, sellerWallet) != 0 {
		t.Fatalf("seller after payout = %d, want 0", h.bal(t, sellerWallet))
	}
	if sends := mock.Sends(); len(sends) != 1 || sends[0].Amount != 1000 || sends[0].ToAddress != "0xSELLERADDR" {
		t.Fatalf("rail sends = %+v", sends)
	}

	// Idempotent on reference: same reference does not pay out twice.
	if _, err := h.core.Payout(context.Background(), facilitator.PayoutRequest{
		SellerWallet: sellerWallet, ToAddress: "0xSELLERADDR", Chain: "base",
		Currency: "USDC", Amount: "1000", Reference: "po-1",
	}); err != nil {
		t.Fatal(err)
	}
	if got := len(mock.Sends()); got != 1 {
		t.Fatalf("rail called %d times for one reference", got)
	}
}

func TestPayout_InsufficientFunds(t *testing.T) {
	h := newCoreHarness(t) // seller has 0
	res, err := h.core.Payout(context.Background(), facilitator.PayoutRequest{
		SellerWallet: sellerWallet, ToAddress: "0xADDR", Chain: "base",
		Currency: "USDC", Amount: "1000", Reference: "po-broke",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != facilitator.PayoutFailed || res.Reason != "insufficient funds" {
		t.Fatalf("payout = %+v, want failed/insufficient funds", res)
	}
}

func TestPayout_RailFailureReverses(t *testing.T) {
	h := newCoreHarness(t, facilitator.WithRail(failingRail{}))
	h.settle(t, false) // seller holds 1000

	res, err := h.core.Payout(context.Background(), facilitator.PayoutRequest{
		SellerWallet: sellerWallet, ToAddress: "0xADDR", Chain: "base",
		Currency: "USDC", Amount: "1000", Reference: "po-fail",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != facilitator.PayoutFailed {
		t.Fatalf("status = %s, want failed", res.Status)
	}
	// Reservation reversed → seller made whole.
	if h.bal(t, sellerWallet) != 1000 {
		t.Fatalf("seller after failed payout = %d, want 1000 (reversed)", h.bal(t, sellerWallet))
	}
}

func TestPayout_WebhookFinalizes(t *testing.T) {
	mock := rail.NewMock()
	mock.Result = rail.StatusPending // async: settle later via webhook
	h := newCoreHarness(t, facilitator.WithRail(mock))
	h.settle(t, false)

	res, err := h.core.Payout(context.Background(), facilitator.PayoutRequest{
		SellerWallet: sellerWallet, ToAddress: "0xADDR", Chain: "base",
		Currency: "USDC", Amount: "1000", Reference: "po-async",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != facilitator.PayoutPending {
		t.Fatalf("status = %s, want pending", res.Status)
	}

	// transfer.success webhook → success, seller stays debited.
	fin, err := h.core.FinalizePayout(context.Background(), "po-async", true)
	if err != nil {
		t.Fatal(err)
	}
	if fin.Status != facilitator.PayoutSuccess {
		t.Fatalf("finalized status = %s, want success", fin.Status)
	}
	if h.bal(t, sellerWallet) != 0 {
		t.Fatalf("seller = %d, want 0 after successful payout", h.bal(t, sellerWallet))
	}
	// Webhook is idempotent.
	if again, _ := h.core.FinalizePayout(context.Background(), "po-async", true); again.Status != facilitator.PayoutSuccess {
		t.Fatalf("re-finalize changed status to %s", again.Status)
	}
}

func TestPayout_WebhookFailureReverses(t *testing.T) {
	mock := rail.NewMock()
	mock.Result = rail.StatusPending
	h := newCoreHarness(t, facilitator.WithRail(mock))
	h.settle(t, false)

	if _, err := h.core.Payout(context.Background(), facilitator.PayoutRequest{
		SellerWallet: sellerWallet, ToAddress: "0xADDR", Chain: "base",
		Currency: "USDC", Amount: "1000", Reference: "po-async-fail",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.core.FinalizePayout(context.Background(), "po-async-fail", false); err != nil {
		t.Fatal(err)
	}
	if h.bal(t, sellerWallet) != 1000 {
		t.Fatalf("seller after failed webhook = %d, want 1000 (reversed)", h.bal(t, sellerWallet))
	}
}
