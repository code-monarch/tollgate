package facilitator_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tollgate/tollgate/internal/facilitator"
	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/settlement"
	tollgate "github.com/tollgate/tollgate/sdk/go"
	"github.com/tollgate/tollgate/x402"
)

const (
	serviceID    = "svc_geocoder"
	sellerWallet = "wallet:seller"
	agentID      = "agt_1"
	agentWallet  = "wallet:agent"
	price        = "1000"
)

// harness wires a facilitator core, a guarded seller endpoint, and a buyer
// identity, mirroring the Milestone 1 exit criterion end to end.
type harness struct {
	core     *facilitator.Core
	seller   *httptest.Server
	agentKey ed25519.PrivateKey
	fpub     ed25519.PublicKey
	meters   int
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
		ID: serviceID, SellerWallet: sellerWallet, Currency: "USDC",
		Network: "base", Asset: "USDC", PayTo: "0xSELLER",
	})
	agentPub, agentPriv, _ := ed25519.GenerateKey(rand.Reader)
	core.RegisterAgent(facilitator.Agent{ID: agentID, Wallet: agentWallet, PublicKey: agentPub})
	if err := core.Fund(context.Background(), agentID, "1000000", "USDC"); err != nil {
		t.Fatal(err)
	}

	h := &harness{core: core, agentKey: agentPriv, fpub: signer.PublicKey()}
	guard := tollgate.Guard(tollgate.Config{
		ServiceID: serviceID, Amount: price, Facilitator: core.AsFacilitator(),
		OnMeter: func(_, _, _ string) { h.meters++ },
	})
	h.seller = httptest.NewServer(guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"q": r.URL.Query().Get("q"), "lat": 37.42})
	})))
	t.Cleanup(h.seller.Close)
	return h
}

func (h *harness) balance(t *testing.T, wallet string) int64 {
	t.Helper()
	b, err := h.core.Balance(context.Background(), wallet, "USDC")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fetchQuote sends an unpaid request and returns the quote from the 402.
func (h *harness) fetchQuote(t *testing.T, url string) x402.Quote {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("unpaid request status = %d, want 402", resp.StatusCode)
	}
	var pr x402.PaymentRequired
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatal(err)
	}
	if len(pr.Accepts) != 1 {
		t.Fatalf("accepts len = %d", len(pr.Accepts))
	}
	return pr.Accepts[0]
}

// payHeader builds a signed X-Payment header for a quote.
func (h *harness) payHeader(t *testing.T, q x402.Quote) string {
	t.Helper()
	if err := x402.VerifyQuote(h.fpub, q); err != nil {
		t.Fatalf("quote signature invalid: %v", err)
	}
	p := x402.Payment{QuoteID: q.QuoteID, Nonce: q.Nonce, AgentID: agentID, PayFrom: agentWallet}
	x402.SignPayment(h.agentKey, &p)
	header, err := x402.EncodePayment(p)
	if err != nil {
		t.Fatal(err)
	}
	return header
}

func paidGet(t *testing.T, url, header string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Payment", header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestFlow_UnpaidToPaidWithLedger is the Milestone 1 exit criterion:
// unpaid → 402 → paid retry → 200 + resource, with a balanced ledger pair.
func TestFlow_UnpaidToPaidWithLedger(t *testing.T) {
	h := newHarness(t)
	url := h.seller.URL + "/geocode?q=hi"

	if got := h.balance(t, agentWallet); got != 1_000_000 {
		t.Fatalf("funded balance = %d", got)
	}

	q := h.fetchQuote(t, url)
	if q.Amount != price {
		t.Fatalf("quote amount = %s, want %s", q.Amount, price)
	}

	resp := paidGet(t, url, h.payHeader(t, q))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("paid request status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Tollgate-Transaction") == "" {
		t.Fatal("missing transaction header on paid response")
	}

	// Double-entry moved exactly the price: buyer down 1000, seller up 1000.
	if got := h.balance(t, agentWallet); got != 999_000 {
		t.Fatalf("agent balance = %d, want 999000", got)
	}
	if got := h.balance(t, sellerWallet); got != 1000 {
		t.Fatalf("seller balance = %d, want 1000", got)
	}
	if h.meters != 1 {
		t.Fatalf("meter events = %d, want 1", h.meters)
	}
}

// TestFlow_IdempotentReplay proves a replayed identical paid request settles
// once: the second call returns 200 but charges nothing.
func TestFlow_IdempotentReplay(t *testing.T) {
	h := newHarness(t)
	url := h.seller.URL + "/geocode?q=hi"

	header := h.payHeader(t, h.fetchQuote(t, url))

	r1 := paidGet(t, url, header)
	r1.Body.Close()
	afterFirst := h.balance(t, agentWallet)

	r2 := paidGet(t, url, header)
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", r2.StatusCode)
	}
	if got := h.balance(t, agentWallet); got != afterFirst {
		t.Fatalf("replay changed balance: %d -> %d (double charge!)", afterFirst, got)
	}
	if got := h.balance(t, sellerWallet); got != 1000 {
		t.Fatalf("seller balance after replay = %d, want 1000", got)
	}
	if h.meters != 1 {
		t.Fatalf("meter events = %d after replay, want 1 (meter once per real charge)", h.meters)
	}
}

// TestFlow_ReplayWithNewQuoteRejected proves the nonce is single-use: paying a
// second, different request with a spent quote's proof is rejected.
func TestFlow_NonceSingleUse(t *testing.T) {
	h := newHarness(t)
	url := h.seller.URL + "/geocode?q=hi"

	q := h.fetchQuote(t, url)
	header := h.payHeader(t, q)

	// First settle consumes the nonce.
	paidGet(t, url, header).Body.Close()

	// Reuse the same proof on a DIFFERENT request path → different request hash,
	// so idempotency does not short-circuit; the consumed nonce must reject it.
	url2 := h.seller.URL + "/geocode?q=different"
	req, _ := http.NewRequest(http.MethodGet, url2, nil)
	req.Header.Set("X-Payment", header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("nonce reuse status = %d, want 402", resp.StatusCode)
	}
}

// TestFlow_OverHTTP runs the same flow but with the seller SDK talking to the
// facilitator over real HTTP (facilitator.Server + tollgate.HTTPClient), so the
// JSON wire path in http.go and client.go is exercised too.
func TestFlow_OverHTTP(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	signer, err := x402.NewSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	core := facilitator.NewCore(ledger.NewMemStore(), signer, settlement.Mock{})
	core.RegisterService(facilitator.Service{
		ID: serviceID, SellerWallet: sellerWallet, Currency: "USDC",
		Network: "base", Asset: "USDC", PayTo: "0xSELLER",
	})
	agentPub, agentPriv, _ := ed25519.GenerateKey(rand.Reader)
	core.RegisterAgent(facilitator.Agent{ID: agentID, Wallet: agentWallet, PublicKey: agentPub})
	if err := core.Fund(context.Background(), agentID, "1000000", "USDC"); err != nil {
		t.Fatal(err)
	}

	// Facilitator over HTTP.
	fac := httptest.NewServer(facilitator.NewServer(core).Routes())
	t.Cleanup(fac.Close)

	// Seller guarded via the HTTP facilitator client.
	guard := tollgate.Guard(tollgate.Config{
		ServiceID: serviceID, Amount: price,
		Facilitator: tollgate.NewHTTPClient(fac.URL),
	})
	seller := httptest.NewServer(guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})))
	t.Cleanup(seller.Close)
	url := seller.URL + "/geocode?q=hi"

	// Unpaid → 402 + quote.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	var pr x402.PaymentRequired
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired || len(pr.Accepts) != 1 {
		t.Fatalf("unpaid status=%d accepts=%d", resp.StatusCode, len(pr.Accepts))
	}
	q := pr.Accepts[0]
	if err := x402.VerifyQuote(signer.PublicKey(), q); err != nil {
		t.Fatalf("quote signature: %v", err)
	}

	// Pay + retry → 200 and a ledger movement.
	p := x402.Payment{QuoteID: q.QuoteID, Nonce: q.Nonce, AgentID: agentID, PayFrom: agentWallet}
	x402.SignPayment(agentPriv, &p)
	header, _ := x402.EncodePayment(p)
	paid := paidGet(t, url, header)
	paid.Body.Close()
	if paid.StatusCode != http.StatusOK {
		t.Fatalf("paid status = %d, want 200", paid.StatusCode)
	}
	if b, _ := core.Balance(context.Background(), sellerWallet, "USDC"); b != 1000 {
		t.Fatalf("seller balance over HTTP = %d, want 1000", b)
	}
}

// TestFlow_InsufficientFunds rejects payment when the wallet can't cover it.
func TestFlow_InsufficientFunds(t *testing.T) {
	h := newHarness(t)
	// Register a broke agent and pay from it.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	h.core.RegisterAgent(facilitator.Agent{ID: "agt_broke", Wallet: "wallet:broke", PublicKey: pub})

	url := h.seller.URL + "/geocode?q=hi"
	q := h.fetchQuote(t, url)
	p := x402.Payment{QuoteID: q.QuoteID, Nonce: q.Nonce, AgentID: "agt_broke", PayFrom: "wallet:broke"}
	x402.SignPayment(priv, &p)
	header, _ := x402.EncodePayment(p)

	resp := paidGet(t, url, header)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("insufficient funds status = %d, want 402", resp.StatusCode)
	}
	if got := h.balance(t, sellerWallet); got != 0 {
		t.Fatalf("seller credited despite rejection: %d", got)
	}
}
