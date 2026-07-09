package marketplace_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tollgate/tollgate/internal/buyer"
	"github.com/tollgate/tollgate/internal/facilitator"
	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/marketplace"
	"github.com/tollgate/tollgate/internal/registry"
	"github.com/tollgate/tollgate/internal/settlement"
	tollgate "github.com/tollgate/tollgate/sdk/go"
	"github.com/tollgate/tollgate/x402"
)

// TestMCP_CallServiceEndToEnd proves the whole stack: an agent discovers a
// service via the marketplace MCP, calls it through call_service, and the x402
// flow actually pays the seller — result + receipt come back to the caller.
func TestMCP_CallServiceEndToEnd(t *testing.T) {
	ctx := context.Background()

	// Facilitator: ledger + signer + mock settlement, with a funded agent.
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
	agentPub, agentPriv, _ := ed25519.GenerateKey(rand.Reader)
	core.RegisterAgent(facilitator.Agent{ID: "agt_1", Wallet: "wallet:agent", PublicKey: agentPub})
	if err := core.Fund(ctx, "agt_1", "1000000", "USDC"); err != nil {
		t.Fatal(err)
	}

	// Seller: a priced GET /geocode guarded by the SDK.
	guard := tollgate.Guard(tollgate.Config{
		ServiceID: "svc_geo", Amount: "1000", Facilitator: core.AsFacilitator(),
	})
	seller := httptest.NewServer(guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"q": r.URL.Query().Get("q"), "lat": 37.42})
	})))
	defer seller.Close()

	// Marketplace registry lists the service, pointing at the seller endpoint.
	store := registry.NewMemStore()
	if err := store.Put(ctx, registry.Service{
		ID: "svc_geo", Name: "Geocoder", Category: "geo",
		Endpoint: seller.URL + "/geocode", SellerWallet: "wallet:seller",
		Pricing: registry.Pricing{Model: "static", Amount: "1000", Currency: "USDC"},
		SLA:     registry.SLA{Uptime: 0.999},
	}); err != nil {
		t.Fatal(err)
	}

	// MCP server backed by a real buyer-driven caller.
	caller := &marketplace.BuyerCaller{
		Store: store,
		Buyer: &buyer.Client{
			AgentID: "agt_1", Wallet: "wallet:agent", PrivateKey: agentPriv,
			FacilitatorPubKey: signer.PublicKey(),
		},
	}
	mcp := marketplace.NewMCP(store, caller)

	// Drive call_service over the MCP JSON-RPC surface.
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"call_service","arguments":{"id":"svc_geo","args":{"q":"hi"}}}}`
	resp, ok := mcp.Handle(ctx, []byte(req))
	if !ok || resp.Error != nil {
		t.Fatalf("call_service failed: ok=%v err=%v", ok, resp.Error)
	}

	// The tool result should carry the 200 body and a receipt/transaction.
	text := firstText(t, resp)
	if !strings.Contains(text, `"status":200`) || !strings.Contains(text, `"lat":37.42`) {
		t.Fatalf("unexpected call result: %s", text)
	}
	if !strings.Contains(text, "rcpt-") || !strings.Contains(text, "txn_") {
		t.Fatalf("call result missing receipt/transaction: %s", text)
	}

	// The seller was actually paid through the ledger.
	if b, _ := core.Balance(ctx, "wallet:seller", "USDC"); b != 1000 {
		t.Fatalf("seller balance = %d, want 1000 (payment did not settle)", b)
	}
}

// firstText pulls the text of the first content block out of a tool result.
func firstText(t *testing.T, resp any) string {
	t.Helper()
	// resp is marketplace.rpcResponse (unexported); marshal via JSON to read it.
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Result.Content) == 0 {
		t.Fatal("no content in tool result")
	}
	return parsed.Result.Content[0].Text
}
