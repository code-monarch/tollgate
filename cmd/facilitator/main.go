// Command facilitator runs the rail as a standalone HTTP server exposing
// /v1/quotes, /v1/payments/verify and /v1/payments/settle (docs/06-api-spec.md).
//
// Milestone 1 uses an in-memory ledger and a mock settlement rail. On startup it
// self-registers one demo service and one funded demo agent so a seller SDK and
// buyer can exercise the flow immediately.
//
//	go run ./cmd/facilitator            # listens on :8080
//	ADDR=:9090 go run ./cmd/facilitator
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log"
	"net/http"
	"os"

	"github.com/tollgate/tollgate/internal/facilitator"
	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/rail"
	"github.com/tollgate/tollgate/internal/rail/bitnob"
	"github.com/tollgate/tollgate/internal/settlement"
	"github.com/tollgate/tollgate/x402"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		log.Fatal(err)
	}
	signer, err := x402.NewSigner(seed)
	if err != nil {
		log.Fatal(err)
	}

	store, storeKind := openStore()
	stableRail, railKind := openRail()
	core := facilitator.NewCore(store, signer, settlement.Mock{}, facilitator.WithRail(stableRail))
	core.RegisterService(facilitator.Service{
		ID: "svc_geocoder", SellerWallet: "wallet:seller_geocoder", Currency: "USDC",
		Network: "base", Asset: "USDC", PayTo: "0xSELLER",
	})
	agentPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	core.RegisterAgent(facilitator.Agent{ID: "agt_demo", Wallet: "wallet:agent_demo", PublicKey: agentPub})
	if err := core.Fund(context.Background(), "agt_demo", "1000000", "USDC"); err != nil {
		log.Fatal(err)
	}

	srv := facilitator.NewServer(core).WithWebhookSecret(os.Getenv("BITNOB_WEBHOOK_SECRET"))
	log.Printf("tollgate facilitator on %s (%s ledger, %s payout rail)", addr, storeKind, railKind)
	if err := http.ListenAndServe(addr, srv.Routes()); err != nil {
		log.Fatal(err)
	}
}

// openRail returns the Bitnob stablecoin rail when BITNOB_CLIENT_ID and
// BITNOB_SECRET are set, otherwise a mock rail. BITNOB_BASE_URL overrides the
// default production host.
func openRail() (rail.Rail, string) {
	clientID := os.Getenv("BITNOB_CLIENT_ID")
	secret := os.Getenv("BITNOB_SECRET")
	if clientID == "" || secret == "" {
		return rail.NewMock(), "mock"
	}
	return bitnob.New(os.Getenv("BITNOB_BASE_URL"), clientID, secret), "bitnob"
}

// openStore returns a Postgres-backed ledger when DATABASE_URL is set (schema
// from db/schema.sql must already be applied), otherwise the in-memory ledger.
func openStore() (ledger.Store, string) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return ledger.NewMemStore(), "in-memory"
	}
	store, err := ledger.NewPGStore(context.Background(), dsn)
	if err != nil {
		log.Fatalf("connect DATABASE_URL: %v", err)
	}
	return store, "postgres"
}
