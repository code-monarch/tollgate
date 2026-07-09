package tollgate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/tollgate/tollgate/x402"
)

// HTTPClient talks to a facilitator over HTTP. It implements Facilitator.
type HTTPClient struct {
	BaseURL string
	HC      *http.Client
}

// NewHTTPClient returns a client for the facilitator at baseURL.
func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{BaseURL: baseURL, HC: http.DefaultClient}
}

// Quote calls POST /v1/quotes.
func (c *HTTPClient) Quote(ctx context.Context, req QuoteRequest) (x402.Quote, error) {
	body := map[string]string{
		"serviceId": req.ServiceID,
		"amount":    req.Amount,
		"resource":  req.Resource,
	}
	var q x402.Quote
	if err := c.post(ctx, "/v1/quotes", body, &q, http.StatusCreated); err != nil {
		return x402.Quote{}, err
	}
	return q, nil
}

// Settle calls POST /v1/payments/settle.
func (c *HTTPClient) Settle(ctx context.Context, req SettleRequest) (SettleResponse, error) {
	body := map[string]any{
		"quoteId":     req.QuoteID,
		"proof":       req.Proof,
		"requestHash": req.RequestHash,
		"escrow":      req.Escrow,
	}
	// Settle returns 200 on success and 402 on protocol rejection; both are
	// non-error HTTP outcomes we decode into SettleResponse.
	raw, status, err := c.do(ctx, "/v1/payments/settle", body)
	if err != nil {
		return SettleResponse{}, err
	}
	switch status {
	case http.StatusOK:
		var r struct {
			TransactionID string `json:"transactionId"`
			Status        string `json:"status"`
			ReceiptID     string `json:"receiptId"`
			Fresh         bool   `json:"fresh"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return SettleResponse{}, err
		}
		return SettleResponse{
			TransactionID: r.TransactionID,
			Status:        r.Status,
			ReceiptID:     r.ReceiptID,
			Settled:       true,
			Fresh:         r.Fresh,
		}, nil
	case http.StatusPaymentRequired:
		var r struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(raw, &r)
		return SettleResponse{Settled: false, Reason: r.Reason}, nil
	default:
		return SettleResponse{}, fmt.Errorf("tollgate: settle unexpected status %d: %s", status, raw)
	}
}

func (c *HTTPClient) post(ctx context.Context, path string, body, out any, wantStatus int) error {
	raw, status, err := c.do(ctx, path, body)
	if err != nil {
		return err
	}
	if status != wantStatus {
		return fmt.Errorf("tollgate: %s status %d: %s", path, status, raw)
	}
	return json.Unmarshal(raw, out)
}

func (c *HTTPClient) do(ctx context.Context, path string, body any) ([]byte, int, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return raw, resp.StatusCode, nil
}
