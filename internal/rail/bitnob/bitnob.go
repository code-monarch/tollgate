// Package bitnob implements the external stablecoin rail against the Bitnob API
// (https://bitnob.dev). It sends stablecoin via POST /api/withdrawals using
// Bitnob's HMAC-SHA256 request signing.
//
// NOTE: this client is written to the documented Bitnob contract but has only
// been exercised against a mock server (bitnob_test.go) — moving real funds
// requires live credentials and a sandbox run. Transfers are asynchronous: Send
// returns a Pending confirmation and Bitnob fires transfer.success/failed
// webhooks (see internal/facilitator webhook handling) to finalize.
package bitnob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tollgate/tollgate/internal/rail"
)

// DefaultBaseURL is the Bitnob production API host.
const DefaultBaseURL = "https://api.bitnob.com"

// Client sends stablecoin via Bitnob. It implements rail.Rail.
type Client struct {
	BaseURL  string
	ClientID string // X-Auth-Client
	Secret   string // HMAC key for request signing
	HC       *http.Client

	// now and nonce are injectable for deterministic tests.
	now   func() time.Time
	nonce func() (string, error)
}

var _ rail.Rail = (*Client)(nil)

// New returns a Bitnob rail client. baseURL defaults to DefaultBaseURL.
func New(baseURL, clientID, secret string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL:  baseURL,
		ClientID: clientID,
		Secret:   secret,
		HC:       &http.Client{Timeout: 30 * time.Second},
		now:      time.Now,
		nonce:    randomNonce,
	}
}

// withdrawalRequest is the POST /api/withdrawals body.
type withdrawalRequest struct {
	ToAddress   string `json:"to_address"`
	Amount      string `json:"amount"` // smallest unit, as a string
	Currency    string `json:"currency"`
	Chain       string `json:"chain"`
	Reference   string `json:"reference"`
	Description string `json:"description,omitempty"`
}

// withdrawalResponse is the (subset of the) Bitnob response we consume.
type withdrawalResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		ID        string `json:"id"`
		Reference string `json:"reference"`
		Status    string `json:"status"`
		CentFees  int64  `json:"centFees"`
	} `json:"data"`
}

// Send implements rail.Rail: POST /api/withdrawals with HMAC auth headers.
func (c *Client) Send(ctx context.Context, t rail.Transfer) (rail.Confirmation, error) {
	if t.Amount <= 0 {
		return rail.Confirmation{}, fmt.Errorf("bitnob: non-positive amount %d", t.Amount)
	}
	body, err := json.Marshal(withdrawalRequest{
		ToAddress:   t.ToAddress,
		Amount:      strconv.FormatInt(t.Amount, 10),
		Currency:    t.Currency,
		Chain:       t.Chain,
		Reference:   t.Reference,
		Description: t.Description,
	})
	if err != nil {
		return rail.Confirmation{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/withdrawals", bytes.NewReader(body))
	if err != nil {
		return rail.Confirmation{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.sign(req, body); err != nil {
		return rail.Confirmation{}, err
	}

	resp, err := c.HC.Do(req)
	if err != nil {
		return rail.Confirmation{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return rail.Confirmation{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return rail.Confirmation{}, fmt.Errorf("bitnob: withdrawal HTTP %d: %s", resp.StatusCode, raw)
	}

	var wr withdrawalResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return rail.Confirmation{}, fmt.Errorf("bitnob: decode response: %w", err)
	}
	if !wr.Status {
		return rail.Confirmation{}, fmt.Errorf("bitnob: withdrawal rejected: %s", wr.Message)
	}
	return rail.Confirmation{
		ProviderRef: wr.Data.ID,
		Status:      mapStatus(wr.Data.Status),
		FeeMinor:    wr.Data.CentFees,
	}, nil
}

// sign attaches Bitnob's HMAC-SHA256 auth headers. The signed message is
// "CLIENT_ID:TIMESTAMP:NONCE:PAYLOAD" where PAYLOAD is the exact request body.
func (c *Client) sign(req *http.Request, body []byte) error {
	ts := strconv.FormatInt(c.now().Unix(), 10)
	nonce, err := c.nonce()
	if err != nil {
		return err
	}
	msg := c.ClientID + ":" + ts + ":" + nonce + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(c.Secret))
	mac.Write([]byte(msg))
	sig := hex.EncodeToString(mac.Sum(nil))

	req.Header.Set("X-Auth-Client", c.ClientID)
	req.Header.Set("X-Auth-Timestamp", ts)
	req.Header.Set("X-Auth-Nonce", nonce)
	req.Header.Set("X-Auth-Signature", sig)
	return nil
}

// mapStatus normalizes Bitnob transfer states to rail.Status. Unknown/absent
// states are treated as Pending — the webhook is the source of truth.
func mapStatus(s string) rail.Status {
	switch s {
	case "success", "successful", "completed", "confirmed":
		return rail.StatusSuccess
	case "failed", "rejected", "cancelled", "canceled":
		return rail.StatusFailed
	default:
		return rail.StatusPending
	}
}

func randomNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
