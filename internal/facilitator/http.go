package facilitator

import (
	"encoding/json"
	"net/http"

	"github.com/tollgate/tollgate/x402"
)

// Server exposes the facilitator Core over HTTP (docs/06-api-spec.md).
type Server struct {
	core          *Core
	webhookSecret string // HMAC secret for verifying rail webhooks (optional)
}

// NewServer wraps a Core in an HTTP server.
func NewServer(c *Core) *Server { return &Server{core: c} }

// WithWebhookSecret enables HMAC-SHA256 verification of inbound rail webhooks.
// When unset, webhooks are accepted without verification (dev only).
func (s *Server) WithWebhookSecret(secret string) *Server {
	s.webhookSecret = secret
	return s
}

// Routes returns the facilitator's HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/quotes", s.handleQuote)
	mux.HandleFunc("POST /v1/payments/verify", s.handleVerify)
	mux.HandleFunc("POST /v1/payments/settle", s.handleSettle)
	mux.HandleFunc("POST /v1/escrow/{transactionId}/release", s.handleRelease)
	mux.HandleFunc("POST /v1/escrow/{transactionId}/refund", s.handleRefund)
	mux.HandleFunc("POST /v1/payouts", s.handlePayout)
	mux.HandleFunc("GET /v1/receipts/{transactionId}", s.handleReceipts)
	mux.HandleFunc("POST /v1/webhooks/bitnob", s.handleBitnobWebhook)
	return mux
}

func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ServiceID string `json:"serviceId"`
		Amount    string `json:"amount"`
		Resource  string `json:"resource"`
	}
	if !decode(w, r, &req) {
		return
	}
	q, err := s.core.IssueQuote(r.Context(), QuoteRequest{
		ServiceID: req.ServiceID, Amount: req.Amount, Resource: req.Resource,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, q)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		QuoteID string `json:"quoteId"`
		Proof   string `json:"proof"`
	}
	if !decode(w, r, &req) {
		return
	}
	p, err := x402.DecodePayment(req.Proof)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	vr, err := s.core.Verify(r.Context(), req.QuoteID, p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid": vr.Valid, "agentId": vr.AgentID, "amount": vr.Amount, "reason": vr.Reason,
	})
}

func (s *Server) handleSettle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		QuoteID     string `json:"quoteId"`
		Proof       string `json:"proof"`
		RequestHash string `json:"requestHash"`
		Escrow      bool   `json:"escrow"`
	}
	if !decode(w, r, &req) {
		return
	}
	p, err := x402.DecodePayment(req.Proof)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.core.Settle(r.Context(), req.QuoteID, p, req.RequestHash, req.Escrow)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !res.Settled {
		// Protocol rejection (expired quote, bad proof, insufficient funds…):
		// 402 with a reason, no settlement, no meter event.
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"reason": res.Reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"transactionId": res.TransactionID,
		"status":        res.Status,
		"receiptId":     res.ReceiptID,
		"fresh":         res.Fresh,
	})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
