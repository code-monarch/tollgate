// Command marketplace serves the Tollgate service registry — the discovery
// layer agents use to find and price paid endpoints (docs/06-api-spec.md).
//
// Two transports over the same registry:
//
//	go run ./cmd/marketplace              # HTTP API on :8081
//	ADDR=:9091 go run ./cmd/marketplace   # HTTP on a custom port
//	go run ./cmd/marketplace -mcp         # MCP server over stdio (for agents)
//
// The MCP server exposes search_services and get_service always; call_service is
// added when a buyer identity is configured via TOLLGATE_AGENT_SEED,
// TOLLGATE_AGENT_ID and TOLLGATE_AGENT_WALLET.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/tollgate/tollgate/internal/buyer"
	"github.com/tollgate/tollgate/internal/marketplace"
	"github.com/tollgate/tollgate/internal/registry"
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
	log.Printf("tollgate marketplace (HTTP) listening on %s", addr)
	if err := http.ListenAndServe(addr, marketplace.NewServer(store).Routes()); err != nil {
		log.Fatal(err)
	}
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
}
