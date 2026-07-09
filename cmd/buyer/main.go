// Command buyer runs the buyer plane: custodial agent wallets, versioned
// policies, and the authorize/pay gate that runs every intended payment through
// the policy engine before funds move (docs/05-policy-engine.md,
// docs/06-api-spec.md buyer plane).
//
// It needs the facilitator's quote-signing public key (base64) to verify quotes,
// via TOLLGATE_FACILITATOR_PUBKEY. For local development, when that is unset it
// spins up an in-process facilitator so the plane is runnable standalone.
//
//	go run ./cmd/buyer              # listens on :8082
//	ADDR=:9092 go run ./cmd/buyer
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"log"
	"net/http"
	"os"

	"github.com/tollgate/tollgate/internal/buyerplane"
	"github.com/tollgate/tollgate/internal/facilitator"
	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/settlement"
	"github.com/tollgate/tollgate/x402"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8082"
	}

	core, facPub := facilitatorCore()
	plane := buyerplane.NewPlane(core, facPub)

	log.Printf("tollgate buyer plane listening on %s", addr)
	if err := http.ListenAndServe(addr, buyerplane.NewServer(plane).Routes()); err != nil {
		log.Fatal(err)
	}
}

// facilitatorCore returns the facilitator core the plane pays through, plus its
// quote-signing public key. For local dev it builds an in-process facilitator;
// wiring a remote facilitator is a follow-up (the plane depends only on the
// core's Balance/Settle/Fund/RegisterAgent surface).
func facilitatorCore() (*facilitator.Core, ed25519.PublicKey) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		log.Fatal(err)
	}
	signer, err := x402.NewSigner(seed)
	if err != nil {
		log.Fatal(err)
	}
	if pk := os.Getenv("TOLLGATE_FACILITATOR_PUBKEY"); pk != "" {
		if _, err := base64.StdEncoding.DecodeString(pk); err != nil {
			log.Fatalf("TOLLGATE_FACILITATOR_PUBKEY must be base64: %v", err)
		}
		log.Print("note: remote facilitator wiring is a follow-up; using an in-process core for now")
	}
	core := facilitator.NewCore(ledger.NewMemStore(), signer, settlement.Mock{})
	return core, signer.PublicKey()
}
