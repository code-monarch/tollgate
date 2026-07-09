// Package tollgate is the seller-side enforcement SDK. Guard is drop-in
// net/http middleware that turns any handler into a paid endpoint speaking the
// x402 flow: an unpaid request gets a 402 + signed quote; a request carrying a
// valid X-Payment proof is verified+settled and then passed through to the
// wrapped handler (docs/02-architecture.md, docs/04-protocol.md).
package tollgate

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/tollgate/tollgate/x402"
)

// QuoteRequest asks the facilitator to price a resource.
type QuoteRequest struct {
	ServiceID string
	Amount    string // minor units, base-10 string
	Resource  string
}

// SettleRequest asks the facilitator to verify a proof and move funds.
type SettleRequest struct {
	QuoteID     string
	Proof       string // base64 X-Payment payload
	RequestHash string // idempotency key
	Escrow      bool
}

// SettleResponse is the outcome of a settle attempt. Settled=false with a
// non-empty Reason means a protocol-level rejection (re-issue a 402), not a
// transport error.
type SettleResponse struct {
	TransactionID string
	Status        string
	ReceiptID     string
	Reason        string
	Settled       bool
	Fresh         bool // true only for a first-time settle, not an idempotent replay
}

// Facilitator is the subset of the rail the seller SDK depends on. It is
// satisfied by an HTTP client (HTTPClient) or by an in-process adapter, so the
// SDK never hard-codes a transport.
type Facilitator interface {
	Quote(ctx context.Context, req QuoteRequest) (x402.Quote, error)
	Settle(ctx context.Context, req SettleRequest) (SettleResponse, error)
}

// Config configures a Guard. Milestone 1 uses static per-route pricing: a fixed
// Amount for every request to the guarded handler.
type Config struct {
	ServiceID   string
	Amount      string // static price in minor units
	Facilitator Facilitator
	// OnMeter is called after a successful paid request, off the critical path,
	// so the seller plane can record a billable event. Optional.
	OnMeter func(serviceID, transactionID, requestHash string)
}

// Guard returns middleware enforcing payment on the wrapped handler.
func Guard(cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("X-Payment")

			// Unpaid request → price it and return a 402 + signed quote.
			if header == "" {
				q, err := cfg.Facilitator.Quote(r.Context(), QuoteRequest{
					ServiceID: cfg.ServiceID,
					Amount:    cfg.Amount,
					Resource:  resourceURL(r),
				})
				if err != nil {
					http.Error(w, "tollgate: quote failed: "+err.Error(), http.StatusBadGateway)
					return
				}
				writePaymentRequired(w, q)
				return
			}

			// Paid retry → decode proof, settle, then pass through on success.
			payment, err := x402.DecodePayment(header)
			if err != nil {
				http.Error(w, "tollgate: "+err.Error(), http.StatusBadRequest)
				return
			}
			reqHash := x402.RequestHash(r.Method, r.URL.Path, r.URL.RawQuery, payment.QuoteID)
			res, err := cfg.Facilitator.Settle(r.Context(), SettleRequest{
				QuoteID:     payment.QuoteID,
				Proof:       header,
				RequestHash: reqHash,
			})
			if err != nil {
				http.Error(w, "tollgate: settle failed: "+err.Error(), http.StatusBadGateway)
				return
			}
			if !res.Settled {
				writeSettleRejected(w, res.Reason)
				return
			}

			w.Header().Set("X-Tollgate-Transaction", res.TransactionID)
			w.Header().Set("X-Tollgate-Receipt", res.ReceiptID)
			next.ServeHTTP(w, r)

			// Meter exactly once per real charge, off the critical path. An
			// idempotent replay (Fresh=false) must not double-count.
			if cfg.OnMeter != nil && res.Fresh {
				cfg.OnMeter(cfg.ServiceID, res.TransactionID, reqHash)
			}
		})
	}
}

func resourceURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.Path
}

func writePaymentRequired(w http.ResponseWriter, q x402.Quote) {
	w.Header().Set("WWW-Authenticate", `X402 realm="tollgate"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(x402.PaymentRequired{
		X402Version: x402.Version,
		Accepts:     []x402.Quote{q},
	})
}

func writeSettleRejected(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
}
