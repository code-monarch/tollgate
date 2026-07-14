package x402

import (
	"crypto/ed25519"
	"crypto/rand"
	"reflect"
	"testing"
	"time"
)

func testSigner(t *testing.T) *Signer {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	s, err := NewSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestQuoteSignRoundTrip(t *testing.T) {
	s := testSigner(t)
	q := Quote{
		QuoteID: "q_1", ServiceID: "svc", Scheme: "exact", Network: "base",
		Asset: "USDC", Amount: "1000", Currency: "USDC", PayTo: "0xabc",
		Resource: "https://x/geocode", Nonce: "n_1",
		ExpiresAt: time.Unix(1000, 0),
	}
	s.SignQuote(&q)
	if err := VerifyQuote(s.PublicKey(), q); err != nil {
		t.Fatalf("valid quote failed verify: %v", err)
	}
	// Tamper with the amount → signature must fail.
	q.Amount = "9999"
	if err := VerifyQuote(s.PublicKey(), q); err == nil {
		t.Fatal("tampered quote should fail verify")
	}
}

func TestPaymentSignRoundTripAndEncoding(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	p := Payment{QuoteID: "q_1", Nonce: "n_1", AgentID: "agt", PayFrom: "wallet:a"}
	SignPayment(priv, &p)
	if err := VerifyPayment(pub, p); err != nil {
		t.Fatalf("valid payment failed verify: %v", err)
	}

	header, err := EncodePayment(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodePayment(header)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, p)
	}

	// Wrong key must fail.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := VerifyPayment(otherPub, p); err == nil {
		t.Fatal("payment verified against wrong key")
	}
}

func TestRequestHash_StableAndSensitive(t *testing.T) {
	a := RequestHash("GET", "/geocode", "q=x", "q_1")
	if a != RequestHash("GET", "/geocode", "q=x", "q_1") {
		t.Fatal("request hash not stable")
	}
	if a == RequestHash("GET", "/geocode", "q=x", "q_2") {
		t.Fatal("request hash ignored quote id")
	}
}

// TestQuote_ExhaustOfferIsSigned proves a seller cannot widen its rights ask after
// the facilitator signed the quote: tampering with the offer breaks the signature.
func TestQuote_ExhaustOfferIsSigned(t *testing.T) {
	s := testSigner(t)
	q := Quote{
		QuoteID: "q_1", ServiceID: "svc_1", Amount: "1000", Currency: "USDC",
		Nonce: "n_1", ExpiresAt: time.Now().Add(time.Minute),
		Exhaust: &ExhaustOffer{
			Optional: []string{"retain"},
			Rebates:  map[string]string{"retain": "50"},
		},
	}
	s.SignQuote(&q)
	if err := VerifyQuote(s.PublicKey(), q); err != nil {
		t.Fatalf("signed quote failed verify: %v", err)
	}

	// Seller sneaks in a training claim after signing.
	tampered := q
	tampered.Exhaust = &ExhaustOffer{
		Required: []string{"train"},
		Optional: []string{"retain"},
		Rebates:  map[string]string{"retain": "50"},
	}
	if err := VerifyQuote(s.PublicKey(), tampered); err == nil {
		t.Fatal("a widened exhaust offer still verified — the rights ask is not protected")
	}

	// Dropping the offer entirely must also break it.
	stripped := q
	stripped.Exhaust = nil
	if err := VerifyQuote(s.PublicKey(), stripped); err == nil {
		t.Fatal("stripping the exhaust offer still verified")
	}
}

// TestPayment_GrantIsSigned proves consent is non-repudiable: nobody can widen or
// strip the buyer's grant in transit without invalidating the agent's signature.
func TestPayment_GrantIsSigned(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	p := Payment{QuoteID: "q_1", Nonce: "n_1", AgentID: "agt_1", PayFrom: "wallet:a",
		Grant: []string{"retain"}}
	SignPayment(priv, &p)
	if err := VerifyPayment(pub, p); err != nil {
		t.Fatalf("signed payment failed verify: %v", err)
	}

	// A seller (or the wire) tries to widen consent to training.
	widened := p
	widened.Grant = []string{"retain", "train"}
	if err := VerifyPayment(pub, widened); err == nil {
		t.Fatal("a widened grant still verified — consent is forgeable")
	}

	// And tries to strip it (e.g. to claim the buyer granted nothing and owes full price).
	stripped := p
	stripped.Grant = nil
	if err := VerifyPayment(pub, stripped); err == nil {
		t.Fatal("a stripped grant still verified")
	}
}

// TestGrant_OrderIndependent: the same rights in a different order are the same
// consent, so a re-ordered grant still verifies.
func TestGrant_OrderIndependent(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	p := Payment{QuoteID: "q_1", Nonce: "n_1", AgentID: "agt_1", PayFrom: "wallet:a",
		Grant: []string{"train", "retain"}}
	SignPayment(priv, &p)

	reordered := p
	reordered.Grant = []string{"retain", "train"}
	if err := VerifyPayment(pub, reordered); err != nil {
		t.Fatalf("re-ordered grant failed verify: %v", err)
	}
}
