package bitnob

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tollgate/tollgate/internal/rail"
)

// TestSend_SignsAndSendsCorrectly stands up a mock Bitnob and asserts the
// request shape and the HMAC-SHA256 auth headers exactly as documented.
func TestSend_SignsAndSendsCorrectly(t *testing.T) {
	const (
		clientID = "client_123"
		secret   = "shhh-secret"
	)

	var gotBody []byte
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/withdrawals" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()

		// Recompute the signature exactly as the server would and compare.
		msg := gotHeaders.Get("X-Auth-Client") + ":" +
			gotHeaders.Get("X-Auth-Timestamp") + ":" +
			gotHeaders.Get("X-Auth-Nonce") + ":" + string(gotBody)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(msg))
		want := hex.EncodeToString(mac.Sum(nil))
		if got := gotHeaders.Get("X-Auth-Signature"); !hmac.Equal([]byte(got), []byte(want)) {
			t.Errorf("signature mismatch:\n got %s\nwant %s", got, want)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":  true,
			"message": "ok",
			"data":    map[string]any{"id": "wd_1", "reference": "ref-1", "status": "processing"},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, clientID, secret)
	conf, err := c.Send(context.Background(), rail.Transfer{
		ToAddress: "0xSELLER", Amount: 2_000_000, Currency: "USDC", Chain: "base",
		Reference: "ref-1", Description: "payout",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Async provider → Pending until webhook.
	if conf.Status != rail.StatusPending || conf.ProviderRef != "wd_1" {
		t.Fatalf("confirmation = %+v", conf)
	}

	// Auth headers present.
	for _, h := range []string{"X-Auth-Client", "X-Auth-Timestamp", "X-Auth-Nonce", "X-Auth-Signature"} {
		if gotHeaders.Get(h) == "" {
			t.Errorf("missing auth header %s", h)
		}
	}
	if gotHeaders.Get("X-Auth-Client") != clientID {
		t.Errorf("X-Auth-Client = %q", gotHeaders.Get("X-Auth-Client"))
	}
	if len(gotHeaders.Get("X-Auth-Nonce")) != 32 { // 16 bytes hex
		t.Errorf("nonce length = %d, want 32 hex chars", len(gotHeaders.Get("X-Auth-Nonce")))
	}

	// Body: amount is the smallest-unit string; chain/currency/reference carried through.
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatal(err)
	}
	if body["amount"] != "2000000" || body["currency"] != "USDC" ||
		body["chain"] != "base" || body["reference"] != "ref-1" || body["to_address"] != "0xSELLER" {
		t.Fatalf("unexpected body: %s", gotBody)
	}
}

// TestSend_RejectsErrorResponse surfaces a provider-level failure as an error.
func TestSend_RejectsErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"status": false, "message": "insufficient balance"})
	}))
	defer srv.Close()

	c := New(srv.URL, "id", "secret")
	if _, err := c.Send(context.Background(), rail.Transfer{
		ToAddress: "0xabc", Amount: 1_000_000, Currency: "USDC", Chain: "base", Reference: "r",
	}); err == nil {
		t.Fatal("expected error for status:false response")
	}
}

func TestSend_RejectsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"status":false,"message":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(srv.URL, "id", "secret")
	if _, err := c.Send(context.Background(), rail.Transfer{
		ToAddress: "0xabc", Amount: 1_000_000, Currency: "USDC", Chain: "base", Reference: "r",
	}); err == nil {
		t.Fatal("expected error for HTTP 401")
	}
}
