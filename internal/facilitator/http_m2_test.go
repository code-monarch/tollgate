package facilitator_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tollgate/tollgate/internal/facilitator"
	"github.com/tollgate/tollgate/internal/rail"
)

// TestHTTP_EscrowReleaseAndReceipts exercises the escrow release + receipts
// endpoints (path values, JSON) end to end over HTTP.
func TestHTTP_EscrowReleaseAndReceipts(t *testing.T) {
	h := newCoreHarness(t)
	srv := httptest.NewServer(facilitator.NewServer(h.core).Routes())
	defer srv.Close()

	res := h.settle(t, true) // escrow held

	rel := httpPost(t, srv.URL+"/v1/escrow/"+res.TransactionID+"/release", "")
	if rel.StatusCode != http.StatusOK {
		t.Fatalf("release status = %d", rel.StatusCode)
	}
	rel.Body.Close()
	if h.bal(t, sellerWallet) != 1000 {
		t.Fatalf("seller not paid after HTTP release: %d", h.bal(t, sellerWallet))
	}

	get, err := http.Get(srv.URL + "/v1/receipts/" + res.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	var body struct {
		Receipts []json.RawMessage `json:"receipts"`
	}
	json.NewDecoder(get.Body).Decode(&body)
	if len(body.Receipts) != 2 {
		t.Fatalf("receipts endpoint returned %d, want 2", len(body.Receipts))
	}
}

// TestHTTP_BitnobWebhookSignature verifies signature enforcement and payout
// finalization over HTTP.
func TestHTTP_BitnobWebhookSignature(t *testing.T) {
	const secret = "whsec_123"
	mock := rail.NewMock()
	mock.Result = rail.StatusPending
	h := newCoreHarness(t, facilitator.WithRail(mock))
	h.settle(t, false) // seller holds 1000

	if _, err := h.core.Payout(context.Background(), facilitator.PayoutRequest{
		SellerWallet: sellerWallet, ToAddress: "0xADDR", Chain: "base",
		Currency: "USDC", Amount: "1000", Reference: "po-http",
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(facilitator.NewServer(h.core).WithWebhookSecret(secret).Routes())
	defer srv.Close()

	payload := `{"event":"transfer.success","data":{"reference":"po-http"}}`

	// Wrong signature → 401, payout stays pending.
	bad := httpPostSigned(t, srv.URL+"/v1/webhooks/bitnob", payload, "deadbeef")
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad-signature webhook status = %d, want 401", bad.StatusCode)
	}
	bad.Body.Close()
	if rec, _ := h.core.PayoutByReference("po-http"); rec.Status != facilitator.PayoutPending {
		t.Fatalf("payout advanced on unsigned webhook: %s", rec.Status)
	}

	// Correct signature → 200, payout finalized.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	good := httpPostSigned(t, srv.URL+"/v1/webhooks/bitnob", payload, hex.EncodeToString(mac.Sum(nil)))
	if good.StatusCode != http.StatusOK {
		t.Fatalf("signed webhook status = %d, want 200", good.StatusCode)
	}
	good.Body.Close()
	if rec, _ := h.core.PayoutByReference("po-http"); rec.Status != facilitator.PayoutSuccess {
		t.Fatalf("payout not finalized: %s", rec.Status)
	}
}

func httpPost(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func httpPostSigned(t *testing.T, url, body, sig string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bitnob-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
