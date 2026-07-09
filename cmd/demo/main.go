// Command demo runs the whole Milestone 1 flow in one process and prints each
// step: an unpaid request gets a 402 + quote, the buyer pays, the retry returns
// 200 + the resource, and replaying the identical paid request is a no-op (no
// double charge).
//
//	go run ./cmd/demo
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/tollgate/tollgate/internal/facilitator"
	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/settlement"
	tollgate "github.com/tollgate/tollgate/sdk/go"
	"github.com/tollgate/tollgate/x402"
)

const (
	serviceID    = "svc_geocoder"
	sellerWallet = "wallet:seller_geocoder"
	agentID      = "agt_demo"
	agentWallet  = "wallet:agent_demo"
	price        = "1000" // 0.001 USDC (6 decimals)
)

func main() {
	ctx := context.Background()

	// --- facilitator: ledger + mock rail + signing key ---
	store := ledger.NewMemStore()
	signer := mustSigner()
	core := facilitator.NewCore(store, signer, settlement.Mock{})
	core.RegisterService(facilitator.Service{
		ID: serviceID, SellerWallet: sellerWallet, Currency: "USDC",
		Network: "base", Asset: "USDC", PayTo: "0xSELLER",
	})

	// --- buyer: agent identity, funded wallet ---
	agentPub, agentPriv := mustKeypair()
	core.RegisterAgent(facilitator.Agent{ID: agentID, Wallet: agentWallet, PublicKey: agentPub})
	if err := core.Fund(ctx, agentID, "1000000", "USDC"); err != nil { // 1 USDC
		log.Fatalf("fund: %v", err)
	}

	// --- seller: a priced GET /geocode guarded by the SDK ---
	guard := tollgate.Guard(tollgate.Config{
		ServiceID:   serviceID,
		Amount:      price,
		Facilitator: core.AsFacilitator(),
		OnMeter: func(svc, tx, hash string) {
			fmt.Printf("   [meter] service=%s tx=%s\n", svc, tx)
		},
	})
	seller := httptest.NewServer(guard(http.HandlerFunc(geocode)))
	defer seller.Close()
	url := seller.URL + "/geocode?q=1600+Amphitheatre+Pkwy"

	fmt.Println("Tollgate — Milestone 1 demo")
	fmt.Printf("agent balance before:  %d USDC-minor\n\n", balance(core, agentWallet))

	// 1) Unpaid request → 402 + signed quote.
	fmt.Println("1) unpaid GET /geocode")
	quote := fetchQuote(url)
	fmt.Printf("   status=402 amount=%s %s quoteId=%s\n", quote.Amount, quote.Currency, quote.QuoteID)

	// Buyer verifies the quote signature before paying, then signs a payment.
	if err := x402.VerifyQuote(signer.PublicKey(), quote); err != nil {
		log.Fatalf("quote signature: %v", err)
	}
	header := signPayment(quote, agentPriv)

	// 2) Pay + retry → 200 + resource + receipt.
	fmt.Println("\n2) pay + retry (same request + X-Payment)")
	resp := paidGet(url, header)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("   status=%d receipt=%s tx=%s\n", resp.StatusCode,
		resp.Header.Get("X-Tollgate-Receipt"), resp.Header.Get("X-Tollgate-Transaction"))
	fmt.Printf("   body=%s", body)
	fmt.Printf("   agent balance now:  %d USDC-minor\n", balance(core, agentWallet))

	// 3) Replay the IDENTICAL paid request → idempotent no-op, no double charge.
	fmt.Println("\n3) replay identical paid request")
	afterFirst := balance(core, agentWallet)
	resp2 := paidGet(url, header)
	resp2.Body.Close()
	fmt.Printf("   status=%d  balance unchanged by replay: %t\n",
		resp2.StatusCode, afterFirst == balance(core, agentWallet))

	fmt.Printf("\nagent balance after:  %d USDC-minor\n", balance(core, agentWallet))
	fmt.Printf("seller balance after: %d USDC-minor\n", balance(core, sellerWallet))
}

// geocode is the origin handler the SDK guards; it runs only once payment settled.
func geocode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"query": r.URL.Query().Get("q"), "lat": 37.4220, "lng": -122.0841,
	})
}

func fetchQuote(url string) x402.Quote {
	resp, err := http.Get(url)
	if err != nil {
		log.Fatalf("unpaid get: %v", err)
	}
	defer resp.Body.Close()
	var pr x402.PaymentRequired
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil || len(pr.Accepts) == 0 {
		log.Fatalf("decode 402: %v", err)
	}
	return pr.Accepts[0]
}

func signPayment(q x402.Quote, priv ed25519.PrivateKey) string {
	p := x402.Payment{QuoteID: q.QuoteID, Nonce: q.Nonce, AgentID: agentID, PayFrom: agentWallet}
	x402.SignPayment(priv, &p)
	header, err := x402.EncodePayment(p)
	if err != nil {
		log.Fatalf("encode payment: %v", err)
	}
	return header
}

func paidGet(url, header string) *http.Response {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Payment", header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("paid get: %v", err)
	}
	return resp
}

func balance(core *facilitator.Core, wallet string) int64 {
	b, err := core.Balance(context.Background(), wallet, "USDC")
	if err != nil {
		log.Fatalf("balance: %v", err)
	}
	return b
}

func mustSigner() *x402.Signer {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		log.Fatal(err)
	}
	s, err := x402.NewSigner(seed)
	if err != nil {
		log.Fatal(err)
	}
	return s
}

func mustKeypair() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	return pub, priv
}
