package x402

import (
	"crypto/ed25519"
	"crypto/rand"
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
	if got != p {
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
