package buyerplane_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tollgate/tollgate/internal/buyerplane"
	"github.com/tollgate/tollgate/internal/facilitator"
	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/policy"
	"github.com/tollgate/tollgate/internal/rights"
	"github.com/tollgate/tollgate/internal/settlement"
	tollgate "github.com/tollgate/tollgate/sdk/go"
	"github.com/tollgate/tollgate/x402"
)

// boundaryHarness is the full buyer-side stack against a seller that has exhaust
// terms: the firm's policy decides what may ever cross the boundary, and the plane
// signs that consent into the payment (docs/08-learning-boundary.md).
type boundaryHarness struct {
	plane  *buyerplane.Plane
	core   *facilitator.Core
	store  ledger.Store
	seller *httptest.Server
}

func newBoundaryHarness(t *testing.T, offer rights.Offer) *boundaryHarness {
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
		ID: "svc_geo", SellerWallet: "wallet:seller", Currency: "USDC",
		Network: "base", Asset: "USDC", PayTo: "0xSELLER",
		Exhaust: offer,
	})
	guard := tollgate.Guard(tollgate.Config{ServiceID: "svc_geo", Amount: "1000", Facilitator: core.AsFacilitator()})
	seller := httptest.NewServer(guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"lat": 37.42})
	})))
	t.Cleanup(seller.Close)

	return &boundaryHarness{
		plane: buyerplane.NewPlane(core, signer.PublicKey()), core: core, store: store, seller: seller,
	}
}

func (h *boundaryHarness) quote(t *testing.T) x402.Quote {
	t.Helper()
	resp, err := http.Get(h.seller.URL + "/geocode")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pr x402.PaymentRequired
	json.NewDecoder(resp.Body).Decode(&pr)
	return pr.Accepts[0]
}

// agent creates a funded agent under the given policy rules.
func (h *boundaryHarness) agent(t *testing.T, rules []policy.Rule) string {
	t.Helper()
	ctx := context.Background()
	a, err := h.plane.CreateAgent(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.plane.Fund(ctx, a.ID, "1000000", "USDC"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.plane.CreatePolicy(ctx, a.ID, policy.Policy{
		Currency: "USDC", Defaults: policy.Defaults{Action: policy.Deny}, Rules: rules,
	}); err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func (h *boundaryHarness) pay(t *testing.T, agentID string) buyerplane.PayResult {
	t.Helper()
	res, err := h.plane.Pay(context.Background(), buyerplane.PayInput{
		AuthorizeInput: buyerplane.AuthorizeInput{AgentID: agentID, TaskID: "t1", Quote: h.quote(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (h *boundaryHarness) buyerBalance(t *testing.T, agentID string) int64 {
	t.Helper()
	b, err := h.plane.Balance(context.Background(), agentID, "USDC")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *boundaryHarness) sellerBalance(t *testing.T) int64 {
	t.Helper()
	b, err := h.core.Balance(context.Background(), "wallet:seller", "USDC")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *boundaryHarness) settled(t *testing.T) []ledger.Transaction {
	t.Helper()
	txns, err := h.store.Transactions(context.Background(), ledger.TxQuery{
		ServiceID: "svc_geo", Statuses: []ledger.Status{ledger.StatusSettled},
	})
	if err != nil {
		t.Fatal(err)
	}
	return txns
}

const spendCeiling = "100000"

// A model that will not serve without training rights is simply unusable under a
// policy that forbids them. Nobody had to read a contract: the rail refuses, before
// any knowledge leaves the building.
func TestPlane_SellerDemandsTraining_PolicyRefuses_Denied(t *testing.T) {
	h := newBoundaryHarness(t, rights.Offer{Required: []rights.Right{rights.Train}})

	// A generous SPEND policy — but it says nothing about exhaust rights, so it
	// grants none. Silence is refusal.
	agentID := h.agent(t, []policy.Rule{
		{ID: "ceiling", Type: policy.TypeAmountCeiling, Max: spendCeiling},
	})

	res := h.pay(t, agentID)

	if res.Paid || res.Decision.Decision != policy.Deny {
		t.Fatalf("a training-demanding seller was paid under a policy that grants nothing: %+v", res)
	}
	if !strings.Contains(res.Decision.Reason, "train") {
		t.Fatalf("reason = %q, want it to name the right that caused the refusal", res.Decision.Reason)
	}
	if got := h.sellerBalance(t); got != 0 {
		t.Fatalf("seller was paid %d despite the boundary refusing", got)
	}
	if got := h.buyerBalance(t, agentID); got != 1_000_000 {
		t.Fatalf("buyer was charged (%d) for a call that never happened", got)
	}
	if n := len(h.settled(t)); n != 0 {
		t.Fatalf("%d transactions settled for a denied call", n)
	}
}

// The same seller, under a policy that DOES permit training: the call goes through
// and the firm is paid a dividend for the knowledge it consented to share.
func TestPlane_PolicyPermitsTraining_GrantsAndEarnsDividend(t *testing.T) {
	h := newBoundaryHarness(t, rights.Offer{
		Required: []rights.Right{rights.Train},
		Rebates:  map[rights.Right]int64{rights.Train: 400},
	})
	agentID := h.agent(t, []policy.Rule{
		{ID: "ceiling", Type: policy.TypeAmountCeiling, Max: spendCeiling},
		{ID: "exhaust", Type: policy.TypeExhaustRights, Values: []string{"train"}},
	})

	res := h.pay(t, agentID)
	if !res.Paid {
		t.Fatalf("call was not paid: %+v", res.Decision)
	}

	// Paid 1000, earned 400 back for the knowledge.
	if got := h.buyerBalance(t, agentID); got != 1_000_000-1000+400 {
		t.Fatalf("buyer balance = %d, want %d (paid 1000, earned a 400 dividend)", got, 1_000_000-600)
	}
	if got := h.sellerBalance(t); got != 600 {
		t.Fatalf("seller balance = %d, want 600 (earned 1000, paid 400 to learn from us)", got)
	}

	txns := h.settled(t)
	if len(txns) != 1 || txns[0].Rebate != 400 {
		t.Fatalf("settled = %+v, want one tx with a 400 dividend", txns)
	}
	if len(txns[0].Rights) != 1 || txns[0].Rights[0] != "train" {
		t.Fatalf("rights that crossed = %v, want [train]", txns[0].Rights)
	}
}

// A policy permitting only `retain` grants exactly that against a seller who also
// wants to train: the firm consents to the lesser right and refuses the greater, on
// the same call, automatically — and turns down the bigger cheque to do it.
func TestPlane_GrantsOnlyWhatPolicyPermits(t *testing.T) {
	h := newBoundaryHarness(t, rights.Offer{
		Optional: []rights.Right{rights.Train, rights.Retain},
		Rebates:  map[rights.Right]int64{rights.Train: 400, rights.Retain: 25},
	})
	agentID := h.agent(t, []policy.Rule{
		{ID: "ceiling", Type: policy.TypeAmountCeiling, Max: spendCeiling},
		{ID: "exhaust", Type: policy.TypeExhaustRights, Values: []string{"retain"}},
	})

	res := h.pay(t, agentID)
	if !res.Paid {
		t.Fatalf("call was not paid: %+v", res.Decision)
	}

	// Only the 25-unit retain dividend was earned. The firm left the far more
	// lucrative 400 on the table because its policy never permitted training:
	// money did not buy the knowledge.
	if got := h.buyerBalance(t, agentID); got != 1_000_000-1000+25 {
		t.Fatalf("buyer balance = %d, want %d (the retain dividend only)", got, 1_000_000-975)
	}

	txns := h.settled(t)
	if len(txns) != 1 {
		t.Fatalf("want 1 settled transaction, got %d", len(txns))
	}
	if len(txns[0].Rights) != 1 || txns[0].Rights[0] != "retain" {
		t.Fatalf("rights that crossed = %v, want only [retain]", txns[0].Rights)
	}
	if txns[0].Rebate != 25 {
		t.Fatalf("dividend = %d, want 25", txns[0].Rebate)
	}
}
