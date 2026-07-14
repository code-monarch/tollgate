// Package x402 implements the wire types and cryptography for the Tollgate x402
// flow: a signed, time-boxed quote (facilitator-signed) and a payment proof
// (agent-signed). It is deliberately dependency-free so both the seller SDK and
// buyer clients can import it.
//
// Money amounts are always strings of integer minor units + a currency code —
// never floats. See docs/03-data-model.md and docs/04-protocol.md.
package x402

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Version is the x402 protocol version this implementation speaks.
const Version = 1

// ExhaustOffer is the seller's claim on the *intelligence exhaust* of a call —
// the prompts, context, traces and corrections that cross the boundary while the
// request is served. Required rights are non-negotiable (the seller will not
// serve without them); optional rights are asked for, with a rebate offered in
// exchange. Because the offer rides inside the facilitator-signed quote, a seller
// cannot change what it asked for after the fact (docs/08-learning-boundary.md).
//
// Rights are plain strings here to keep x402 dependency-free; internal/rights
// holds the vocabulary and the semantics.
type ExhaustOffer struct {
	Required []string          `json:"required,omitempty"` // will not serve without these
	Optional []string          `json:"optional,omitempty"` // would like these
	Rebates  map[string]string `json:"rebates,omitempty"`  // right -> minor units returned if granted
}

// Quote is a signed, time-boxed price challenge returned inside a 402 response.
// The signature is produced by the facilitator over the canonical encoding of
// every other field (see quoteSigningBytes); it prevents tampering and lets any
// party verify the price without trusting the seller edge.
type Quote struct {
	QuoteID   string    `json:"quoteId"`
	ServiceID string    `json:"serviceId"`
	Scheme    string    `json:"scheme"`   // e.g. "exact"
	Network   string    `json:"network"`  // e.g. "base"
	Asset     string    `json:"asset"`    // e.g. "USDC"
	Amount    string    `json:"amount"`   // minor units, base-10 string
	Currency  string    `json:"currency"` // e.g. "USDC"
	PayTo     string    `json:"payTo"`    // seller on-chain address (display)
	Resource  string    `json:"resource"` // the guarded URL
	Nonce     string    `json:"nonce"`    // single-use
	ExpiresAt time.Time `json:"expiresAt"`

	// Exhaust is the seller's rights ask. Nil means the seller claims nothing:
	// the call is served and the exhaust stays entirely on the buyer's side.
	Exhaust   *ExhaustOffer `json:"exhaust,omitempty"`
	Signature string        `json:"signature"` // facilitator ed25519 sig, base64
}

// PaymentRequired is the JSON body of a 402 response. accepts is a list so a
// seller can offer multiple currencies/networks for the same resource.
type PaymentRequired struct {
	X402Version int     `json:"x402Version"`
	Accepts     []Quote `json:"accepts"`
}

// Payment is the decoded X-Payment header: the buyer's proof that it authorized
// paying a specific quote. Signature is the agent's ed25519 signature over the
// quote id, nonce, agent id, paying wallet and the rights granted.
type Payment struct {
	QuoteID string `json:"quoteId"`
	Nonce   string `json:"nonce"`
	AgentID string `json:"agentId"`
	PayFrom string `json:"payFrom"` // buyer wallet id

	// Grant is the buyer's consent: the exhaust rights it permits for this call.
	// It is covered by the agent's signature, so consent is non-repudiable. An
	// empty grant means nothing crosses the boundary — silence never grants.
	Grant     []string `json:"grant,omitempty"`
	Signature string   `json:"signature"` // agent ed25519 sig, base64
}

// ---- facilitator quote signing ----

// Signer holds the facilitator's ed25519 key and signs quotes.
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// NewSigner derives a deterministic signer from a 32-byte seed. Determinism
// keeps tests and multi-instance facilitators reproducible.
func NewSigner(seed []byte) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("x402: signer seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{priv: priv, pub: priv.Public().(ed25519.PublicKey)}, nil
}

// PublicKey returns the facilitator's verifying key.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.pub }

// SignQuote fills q.Signature over the canonical encoding of q's other fields.
func (s *Signer) SignQuote(q *Quote) {
	sig := ed25519.Sign(s.priv, quoteSigningBytes(*q))
	q.Signature = base64.StdEncoding.EncodeToString(sig)
}

// SignMessage returns the base64 ed25519 signature of msg under the
// facilitator key. Used for receipts and other facilitator-signed artifacts.
func (s *Signer) SignMessage(msg []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, msg))
}

// VerifyMessage checks a base64 ed25519 signature of msg against pub.
func VerifyMessage(pub ed25519.PublicKey, msg []byte, sigB64 string) error {
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("x402: bad signature encoding: %w", err)
	}
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("x402: invalid signature")
	}
	return nil
}

// VerifyQuote checks a quote's facilitator signature against pub.
func VerifyQuote(pub ed25519.PublicKey, q Quote) error {
	sig, err := base64.StdEncoding.DecodeString(q.Signature)
	if err != nil {
		return fmt.Errorf("x402: bad quote signature encoding: %w", err)
	}
	if !ed25519.Verify(pub, quoteSigningBytes(q), sig) {
		return errors.New("x402: invalid quote signature")
	}
	return nil
}

// quoteSigningBytes is the canonical, signature-free encoding of a quote. Field
// order is fixed; timestamps use RFC3339Nano in UTC so the bytes are stable. The
// exhaust offer is included, so the seller's rights ask is as tamper-proof as its
// price.
func quoteSigningBytes(q Quote) []byte {
	var b strings.Builder
	fields := []string{
		q.QuoteID, q.ServiceID, q.Scheme, q.Network, q.Asset,
		q.Amount, q.Currency, q.PayTo, q.Resource, q.Nonce,
		q.ExpiresAt.UTC().Format(time.RFC3339Nano),
		exhaustSigningString(q.Exhaust),
	}
	for i, f := range fields {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(f)
	}
	return []byte(b.String())
}

// exhaustSigningString canonically encodes an exhaust offer: each list sorted and
// the rebate map emitted in sorted key order, so the bytes are stable regardless
// of map iteration or caller ordering. A nil offer (the seller claims nothing)
// encodes to the empty string, which keeps signatures stable for callers that
// never touch rights at all.
func exhaustSigningString(o *ExhaustOffer) string {
	if o == nil {
		return ""
	}
	rebates := make([]string, 0, len(o.Rebates))
	for right, amount := range o.Rebates {
		rebates = append(rebates, right+"="+amount)
	}
	sort.Strings(rebates)
	return strings.Join(SortedRights(o.Required), ",") + ";" +
		strings.Join(SortedRights(o.Optional), ",") + ";" +
		strings.Join(rebates, ",")
}

// SortedRights returns a sorted copy of rs with duplicates removed, so that two
// grants naming the same rights in a different order sign to the same bytes.
func SortedRights(rs []string) []string {
	if len(rs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(rs))
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// ---- agent payment signing ----

// SignPayment fills p.Signature with the agent's signature over p's other fields.
func SignPayment(priv ed25519.PrivateKey, p *Payment) {
	sig := ed25519.Sign(priv, paymentSigningBytes(*p))
	p.Signature = base64.StdEncoding.EncodeToString(sig)
}

// VerifyPayment checks a payment proof's agent signature against pub.
func VerifyPayment(pub ed25519.PublicKey, p Payment) error {
	sig, err := base64.StdEncoding.DecodeString(p.Signature)
	if err != nil {
		return fmt.Errorf("x402: bad payment signature encoding: %w", err)
	}
	if !ed25519.Verify(pub, paymentSigningBytes(p), sig) {
		return errors.New("x402: invalid payment signature")
	}
	return nil
}

// paymentSigningBytes covers the granted rights as well as the payment fields, so
// a buyer's consent cannot be forged, widened or stripped in transit: altering the
// grant invalidates the agent's signature.
func paymentSigningBytes(p Payment) []byte {
	return []byte(p.QuoteID + "\n" + p.Nonce + "\n" + p.AgentID + "\n" + p.PayFrom +
		"\n" + strings.Join(SortedRights(p.Grant), ","))
}

// ---- X-Payment header codec ----

// EncodePayment serializes a payment to the base64 JSON form carried in X-Payment.
func EncodePayment(p Payment) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// DecodePayment parses an X-Payment header value.
func DecodePayment(header string) (Payment, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header))
	if err != nil {
		return Payment{}, fmt.Errorf("x402: bad X-Payment encoding: %w", err)
	}
	var p Payment
	if err := json.Unmarshal(raw, &p); err != nil {
		return Payment{}, fmt.Errorf("x402: bad X-Payment json: %w", err)
	}
	return p, nil
}

// RequestHash is the idempotency key for a paid request: a stable hash over the
// canonicalized method, path, query and quote id. Retries of the same request
// against the same quote produce the same hash, so charges are never doubled.
func RequestHash(method, path, rawQuery, quoteID string) string {
	h := sha256.New()
	io.WriteString(h, method+"\n"+path+"\n"+rawQuery+"\n"+quoteID)
	return hex.EncodeToString(h.Sum(nil))
}
