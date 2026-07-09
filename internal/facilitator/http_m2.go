package facilitator

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/tollgate/tollgate/internal/receipt"
)

// POST /v1/escrow/{transactionId}/release
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	res, err := s.core.Release(r.Context(), r.PathValue("transactionId"))
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"transactionId": res.TransactionID, "status": string(res.Status), "receiptId": res.ReceiptID,
	})
}

// POST /v1/escrow/{transactionId}/refund
func (s *Server) handleRefund(w http.ResponseWriter, r *http.Request) {
	res, err := s.core.Refund(r.Context(), r.PathValue("transactionId"))
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"transactionId": res.TransactionID, "status": string(res.Status),
	})
}

// POST /v1/payouts
func (s *Server) handlePayout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SellerWallet string `json:"sellerWallet"`
		ToAddress    string `json:"toAddress"`
		Chain        string `json:"chain"`
		Currency     string `json:"currency"`
		Amount       string `json:"amount"`
		Reference    string `json:"reference"`
	}
	if !decode(w, r, &req) {
		return
	}
	res, err := s.core.Payout(r.Context(), PayoutRequest{
		SellerWallet: req.SellerWallet, ToAddress: req.ToAddress, Chain: req.Chain,
		Currency: req.Currency, Amount: req.Amount, Reference: req.Reference,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusAccepted // async: pending until the webhook confirms
	if res.Status == PayoutFailed {
		status = http.StatusPaymentRequired
	}
	writeJSON(w, status, map[string]any{
		"reference": res.Reference, "transactionId": res.TransactionID,
		"providerRef": res.ProviderRef, "status": string(res.Status), "reason": res.Reason,
	})
}

// GET /v1/receipts/{transactionId}
func (s *Server) handleReceipts(w http.ResponseWriter, r *http.Request) {
	receipts := s.core.Receipts(r.Context(), r.PathValue("transactionId"))
	if receipts == nil {
		receipts = []receipt.Receipt{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": receipts})
}

// POST /v1/webhooks/bitnob — finalize a pending payout on transfer.success /
// transfer.failed. Verifies an HMAC-SHA256 body signature when a secret is set.
func (s *Server) handleBitnobWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.webhookSecret != "" && !validWebhookSignature(s.webhookSecret, body, r.Header.Get("X-Bitnob-Signature")) {
		writeErr(w, http.StatusUnauthorized, "invalid webhook signature")
		return
	}

	var evt struct {
		Event string `json:"event"`
		Data  struct {
			Reference string `json:"reference"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &evt); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if evt.Data.Reference == "" {
		writeErr(w, http.StatusBadRequest, "missing data.reference")
		return
	}

	var success bool
	switch evt.Event {
	case "transfer.success":
		success = true
	case "transfer.failed":
		success = false
	default:
		// Acknowledge unrelated events without acting on them.
		writeJSON(w, http.StatusOK, map[string]any{"ignored": evt.Event})
		return
	}

	res, err := s.core.FinalizePayout(r.Context(), evt.Data.Reference, success)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reference": res.Reference, "status": string(res.Status),
	})
}

func validWebhookSignature(secret string, body []byte, provided string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(provided))
}
