// Command marketplace serves the Tollgate service registry — the discovery
// layer agents use to find and price paid endpoints (docs/06-api-spec.md).
//
// Two transports over the same registry:
//
//	go run ./cmd/marketplace              # HTTP API on :8081
//	ADDR=:9091 go run ./cmd/marketplace   # HTTP on a custom port
//	go run ./cmd/marketplace -mcp         # MCP server over stdio (for agents)
//
// The HTTP transport also serves the seller analytics + dynamic-pricing plane
// (GET /v1/analytics/services/{id}, GET /v1/pricing/services/{id}) over a shared
// in-memory ledger seeded with demo traffic so the endpoints return real data.
//
// The MCP server exposes search_services and get_service always; call_service is
// added when a buyer identity is configured via TOLLGATE_AGENT_SEED,
// TOLLGATE_AGENT_ID and TOLLGATE_AGENT_WALLET.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/tollgate/tollgate/internal/analytics"
	"github.com/tollgate/tollgate/internal/buyer"
	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/marketplace"
	"github.com/tollgate/tollgate/internal/registry"
	"github.com/tollgate/tollgate/internal/rights"
)

func main() {
	mcpMode := flag.Bool("mcp", false, "run as an MCP server over stdio instead of HTTP")
	flag.Parse()

	store := registry.NewMemStore()
	seedDemoService(store)

	if *mcpMode {
		runMCP(store)
		return
	}

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8081"
	}

	led := ledger.NewMemStore()
	seedDemoLedger(led)
	handler := routes(store, led)

	log.Printf("tollgate marketplace (HTTP + analytics) listening on %s", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}

// routes composes the marketplace catalog handler with the analytics + pricing
// handler over one mux. The analytics mux carries fully-qualified patterns, so
// delegating the /v1/analytics/ and /v1/pricing/ prefixes to it matches cleanly.
func routes(store registry.Store, led ledger.Store) http.Handler {
	ana := analytics.NewServer(led, store).Routes()
	root := http.NewServeMux()
	root.Handle("/v1/analytics/", ana)
	root.Handle("/v1/pricing/", ana)
	root.Handle("/", marketplace.NewServer(store).Routes())
	return root
}

func runMCP(store registry.Store) {
	caller := buildCaller(store) // nil unless a buyer identity is configured
	mcp := marketplace.NewMCP(store, caller)
	// Log to stderr so stdout stays a clean JSON-RPC channel.
	log.SetOutput(os.Stderr)
	if caller == nil {
		log.Print("marketplace MCP: discovery only (set TOLLGATE_AGENT_* to enable call_service)")
	} else {
		log.Print("marketplace MCP: call_service enabled")
	}
	if err := mcp.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// buildCaller wires a buyer-driven call_service when an agent identity is set.
func buildCaller(store registry.Store) marketplace.Caller {
	seedHex := os.Getenv("TOLLGATE_AGENT_SEED")
	agentID := os.Getenv("TOLLGATE_AGENT_ID")
	wallet := os.Getenv("TOLLGATE_AGENT_WALLET")
	if seedHex == "" || agentID == "" || wallet == "" {
		return nil
	}
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != ed25519.SeedSize {
		log.Fatalf("marketplace: TOLLGATE_AGENT_SEED must be %d hex-encoded bytes", ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)

	b := &buyer.Client{AgentID: agentID, Wallet: wallet, PrivateKey: priv}
	if pk := os.Getenv("TOLLGATE_FACILITATOR_PUBKEY"); pk != "" {
		raw, err := base64.StdEncoding.DecodeString(pk)
		if err != nil {
			log.Fatalf("marketplace: TOLLGATE_FACILITATOR_PUBKEY must be base64: %v", err)
		}
		b.FacilitatorPubKey = ed25519.PublicKey(raw)
	}
	return &marketplace.BuyerCaller{Store: store, Buyer: b}
}

func seedDemoService(store *registry.MemStore) {
	_ = store.Put(context.Background(), registry.Service{
		ID: "svc_geocoder", Name: "Geocoder", Description: "Convert an address to lat/lng.",
		Category: "geo", Endpoint: "http://localhost:8080/geocode", SellerWallet: "wallet:seller_geocoder",
		Pricing: registry.Pricing{Model: "static", Amount: "1000", Currency: "USDC"},
		SLA:     registry.SLA{Uptime: 0.999, LatencyMs: 40},
	})
	// A dynamic-priced service so GET /v1/pricing/services/{id} shows surge: base
	// 500, tuned for 10 calls/hour, up to ±50%. It also ASKS to learn from callers
	// and pays for the privilege — decline and you pay list price.
	_ = store.Put(context.Background(), registry.Service{
		ID: "svc_summarizer", Name: "Summarizer", Description: "Summarize a document.",
		Category: "nlp", Endpoint: "http://localhost:8080/summarize", SellerWallet: "wallet:seller_summarizer",
		Pricing: registry.Pricing{
			Model: "dynamic", Amount: "500", Currency: "USDC",
			Floor: 250, Ceiling: 750, TargetRate: 10, MaxSurge: 0.5,
		},
		SLA: registry.SLA{Uptime: 0.995, LatencyMs: 120},
		Exhaust: rights.Offer{
			Optional: []rights.Right{rights.Train, rights.Retain},
			Rebates:  map[rights.Right]int64{rights.Train: 150, rights.Retain: 25},
		},
	})
	// A model that will not serve unless it may train on you — the status quo the
	// learning boundary exists to make visible and refusable. It is discoverable,
	// but `?excludeRequired=train` filters it out, and a policy that does not grant
	// `train` will refuse to pay it at all (docs/08-learning-boundary.md).
	_ = store.Put(context.Background(), registry.Service{
		ID: "svc_oracle", Name: "Oracle LLM", Description: "Frontier model. Trains on your queries.",
		Category: "nlp", Endpoint: "http://localhost:8080/oracle", SellerWallet: "wallet:seller_oracle",
		Pricing: registry.Pricing{Model: "static", Amount: "200", Currency: "USDC"},
		SLA:     registry.SLA{Uptime: 0.999, LatencyMs: 300},
		Exhaust: rights.Offer{
			Required: []rights.Right{rights.Train, rights.ImproveMemory},
		},
	})
}

// seedDemoLedger posts real, balanced settled transactions so the analytics and
// pricing endpoints return meaningful data from a fresh process. The geocoder is
// sold at two price points across two callers (enough to fit an elasticity
// curve); the summarizer gets recent traffic to drive its dynamic surge.
func seedDemoLedger(led ledger.Store) {
	now := time.Now().UTC()

	// Geocoder: cheaper price sells more (elastic demand), two distinct callers.
	settle(led, "svc_geocoder", "agt_alpha", "wallet:agent_alpha", "wallet:seller_geocoder", 1000, 12, now.Add(-50*time.Minute))
	settle(led, "svc_geocoder", "agt_beta", "wallet:agent_beta", "wallet:seller_geocoder", 1000, 6, now.Add(-40*time.Minute))
	settle(led, "svc_geocoder", "agt_alpha", "wallet:agent_alpha", "wallet:seller_geocoder", 1500, 4, now.Add(-30*time.Minute))

	// Summarizer: 14 recent calls vs a target of 10 → the pricing engine surges.
	settle(led, "svc_summarizer", "agt_alpha", "wallet:agent_alpha", "wallet:seller_summarizer", 500, 14, now.Add(-20*time.Minute))
}

// settle posts n balanced settled transactions (agent debit, seller credit) at a
// fixed price, staggered a second apart from base so each has a distinct id/time.
func settle(led ledger.Store, serviceID, agentID, buyerWallet, sellerWallet string, price int64, n int, base time.Time) {
	for i := 0; i < n; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		id := newID("txn")
		_, _, err := led.Post(context.Background(), ledger.Posting{
			Tx: ledger.Transaction{
				ID: id, AgentID: agentID, ServiceID: serviceID, Amount: price, Currency: "USDC",
				Status: ledger.StatusSettled, RequestHash: "seed:" + id, CreatedAt: at, SettledAt: &at,
			},
			Entries: []ledger.Entry{
				{WalletID: buyerWallet, Direction: ledger.Debit, Amount: price, Currency: "USDC", CreatedAt: at},
				{WalletID: sellerWallet, Direction: ledger.Credit, Amount: price, Currency: "USDC", CreatedAt: at},
			},
		})
		if err != nil {
			log.Fatalf("seed ledger: %v", err)
		}
	}
}

// newID returns a prefixed random id for seed transactions.
func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Fatalf("crypto/rand: %v", err)
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
