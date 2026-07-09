package buyerplane_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tollgate/tollgate/internal/buyerplane"
	"github.com/tollgate/tollgate/internal/facilitator"
	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/policy"
	"github.com/tollgate/tollgate/internal/settlement"
	tollgate "github.com/tollgate/tollgate/sdk/go"
	"github.com/tollgate/tollgate/x402"
)

// harness wires a facilitator + a guarded seller + a buyer plane. The seller is
// priced at 1000; the buyer plane holds the agent key and gates every payment
// through the policy engine.
type harness struct {
	plane  *buyerplane.Plane
	core   *facilitator.Core
	seller *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	signer, err := x402.NewSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	core := facilitator.NewCore(ledger.NewMemStore(), signer, settlement.Mock{})
	core.RegisterService(facilitator.Service{
		ID: "svc_geo", SellerWallet: "wallet:seller", Currency: "USDC",
		Network: "base", Asset: "USDC", PayTo: "0xSELLER",
	})
	guard := tollgate.Guard(tollgate.Config{ServiceID: "svc_geo", Amount: "1000", Facilitator: core.AsFacilitator()})
	seller := httptest.NewServer(guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"lat": 37.42})
	})))
	t.Cleanup(seller.Close)

	return &harness{plane: buyerplane.NewPlane(core, signer.PublicKey()), core: core, seller: seller}
}

// quote fetches a 402 from the seller and returns its quote.
func (h *harness) quote(t *testing.T) x402.Quote {
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

func (h *harness) newFundedAgent(t *testing.T) string {
	t.Helper()
	a, err := h.plane.CreateAgent(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.plane.Fund(context.Background(), a.ID, "1000000", "USDC"); err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func setPolicy(t *testing.T, h *harness, agentID string, rules []policy.Rule) {
	t.Helper()
	_, err := h.plane.CreatePolicy(context.Background(), agentID, policy.Policy{
		Currency: "USDC", Defaults: policy.Defaults{Action: policy.Deny}, Rules: rules,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPay_NoPolicy_Denied(t *testing.T) {
	h := newHarness(t)
	agentID := h.newFundedAgent(t)

	res, err := h.plane.Pay(context.Background(), buyerplane.PayInput{
		AuthorizeInput: buyerplane.AuthorizeInput{AgentID: agentID, TaskID: "t1", Quote: h.quote(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Paid || res.Decision.Decision != policy.Deny {
		t.Fatalf("no policy must deny, got %+v", res)
	}
	if b, _ := h.core.Balance(context.Background(), "wallet:seller", "USDC"); b != 0 {
		t.Fatalf("seller paid despite deny: %d", b)
	}
}

func TestPay_WithinCeiling_Allows(t *testing.T) {
	h := newHarness(t)
	agentID := h.newFundedAgent(t)
	setPolicy(t, h, agentID, []policy.Rule{
		{ID: "ceil", Type: policy.TypeAmountCeiling, Max: "5000"},
		{ID: "hosts", Type: policy.TypeAllowlist, Field: "resource_host", Values: []string{"127.0.0.1"}},
	})

	res, err := h.plane.Pay(context.Background(), buyerplane.PayInput{
		AuthorizeInput: buyerplane.AuthorizeInput{AgentID: agentID, TaskID: "t1", Quote: h.quote(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Paid || res.Status != http.StatusOK {
		t.Fatalf("within-policy pay should succeed, got %+v", res)
	}
	if b, _ := h.core.Balance(context.Background(), "wallet:seller", "USDC"); b != 1000 {
		t.Fatalf("seller balance = %d, want 1000", b)
	}
}

func TestPay_OverCeiling_DeniedNoCharge(t *testing.T) {
	h := newHarness(t)
	agentID := h.newFundedAgent(t)
	setPolicy(t, h, agentID, []policy.Rule{{ID: "ceil", Type: policy.TypeAmountCeiling, Max: "500"}})

	res, err := h.plane.Pay(context.Background(), buyerplane.PayInput{
		AuthorizeInput: buyerplane.AuthorizeInput{AgentID: agentID, TaskID: "t1", Quote: h.quote(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Paid || res.Decision.Decision != policy.Deny {
		t.Fatalf("over-ceiling must deny, got %+v", res)
	}
	if b, _ := h.core.Balance(context.Background(), "wallet:seller", "USDC"); b != 0 {
		t.Fatalf("seller charged on denied pay: %d", b)
	}
}

func TestPay_TaskBudget_ExhaustsAfterCalls(t *testing.T) {
	h := newHarness(t)
	agentID := h.newFundedAgent(t)
	// Task budget 2500 → allows two 1000 calls, denies the third (3000 > 2500).
	setPolicy(t, h, agentID, []policy.Rule{
		{ID: "ceil", Type: policy.TypeAmountCeiling, Max: "5000"},
		{ID: "task", Type: policy.TypeBudget, Scope: "task", Window: "task", Max: "2500"},
	})

	pay := func() buyerplane.PayResult {
		res, err := h.plane.Pay(context.Background(), buyerplane.PayInput{
			AuthorizeInput: buyerplane.AuthorizeInput{AgentID: agentID, TaskID: "t1", Quote: h.quote(t)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if !pay().Paid || !pay().Paid {
		t.Fatal("first two calls should pay")
	}
	if third := pay(); third.Paid || third.Decision.Decision != policy.Deny {
		t.Fatalf("third call should be denied by task budget, got %+v", third)
	}
	if b, _ := h.core.Balance(context.Background(), "wallet:seller", "USDC"); b != 2000 {
		t.Fatalf("seller balance = %d, want 2000 (two paid calls)", b)
	}
}

func TestPay_NeedsApproval_UnlocksAfterResolve(t *testing.T) {
	h := newHarness(t)
	agentID := h.newFundedAgent(t)
	setPolicy(t, h, agentID, []policy.Rule{
		{ID: "ceil", Type: policy.TypeAmountCeiling, Max: "5000"},
		{ID: "approve", Type: policy.TypeApproval, Threshold: "1000"}, // 1000 >= 1000 → approval
	})

	q := h.quote(t)
	first, err := h.plane.Pay(context.Background(), buyerplane.PayInput{
		AuthorizeInput: buyerplane.AuthorizeInput{AgentID: agentID, TaskID: "t1", Quote: q},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Paid || first.Decision.Decision != policy.NeedsApproval || first.Decision.ApprovalRequestID == "" {
		t.Fatalf("should need approval with an id, got %+v", first)
	}

	// Human approves.
	if _, ok := h.plane.ResolveApproval(context.Background(), first.Decision.ApprovalRequestID, true); !ok {
		t.Fatal("resolve failed")
	}

	// Retry with the approval id and a fresh quote → pays.
	second, err := h.plane.Pay(context.Background(), buyerplane.PayInput{
		AuthorizeInput:    buyerplane.AuthorizeInput{AgentID: agentID, TaskID: "t1", Quote: h.quote(t)},
		ApprovalRequestID: first.Decision.ApprovalRequestID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Paid {
		t.Fatalf("approved retry should pay, got %+v", second)
	}
}

func TestHTTP_AuthorizeEndpoint(t *testing.T) {
	h := newHarness(t)
	agentID := h.newFundedAgent(t)
	setPolicy(t, h, agentID, []policy.Rule{{ID: "ceil", Type: policy.TypeAmountCeiling, Max: "500"}})

	srv := httptest.NewServer(buyerplane.NewServer(h.plane).Routes())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"agentId": agentID, "taskId": "t1", "quote": h.quote(t)})
	resp, err := http.Post(srv.URL+"/v1/authorize", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var dec buyerplane.Decision
	json.NewDecoder(resp.Body).Decode(&dec)
	if dec.Decision != policy.Deny {
		t.Fatalf("authorize over ceiling should deny, got %+v", dec)
	}
}
