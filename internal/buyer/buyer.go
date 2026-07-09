// Package buyer is a minimal buyer-side client for the demo and tests. It runs
// the client half of the x402 flow: send an unpaid request, and on a 402 verify
// the quote signature, sign a payment with the agent key, and retry. The full
// buyer plane (wallet, policy engine) arrives in Milestone 4.
package buyer

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tollgate/tollgate/x402"
)

// Client pays for resources using a single agent identity.
type Client struct {
	AgentID    string
	Wallet     string
	PrivateKey ed25519.PrivateKey
	// FacilitatorPubKey verifies quote signatures before paying. Optional; if
	// nil, the quote signature is trusted (not recommended).
	FacilitatorPubKey ed25519.PublicKey
	HC                *http.Client
}

// Get fetches url, transparently handling a single 402 challenge.
func (c *Client) Get(url string) (*http.Response, error) {
	hc := c.HC
	if hc == nil {
		hc = http.DefaultClient
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPaymentRequired {
		return resp, nil // free, or an error we pass through
	}

	var pr x402.PaymentRequired
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("buyer: decode 402 body: %w", err)
	}
	resp.Body.Close()
	if len(pr.Accepts) == 0 {
		return nil, fmt.Errorf("buyer: 402 with no accepts")
	}
	quote := pr.Accepts[0]

	if c.FacilitatorPubKey != nil {
		if err := x402.VerifyQuote(c.FacilitatorPubKey, quote); err != nil {
			return nil, fmt.Errorf("buyer: refusing to pay unverified quote: %w", err)
		}
	}

	payment := x402.Payment{
		QuoteID: quote.QuoteID,
		Nonce:   quote.Nonce,
		AgentID: c.AgentID,
		PayFrom: c.Wallet,
	}
	x402.SignPayment(c.PrivateKey, &payment)
	header, err := x402.EncodePayment(payment)
	if err != nil {
		return nil, err
	}

	paid, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	paid.Header.Set("X-Payment", header)
	return hc.Do(paid)
}
