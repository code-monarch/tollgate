package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	"github.com/tollgate/tollgate/internal/buyer"
	"github.com/tollgate/tollgate/internal/registry"
)

// BuyerCaller implements Caller by resolving a service's endpoint from the
// registry and invoking it through a buyer client, which runs the full x402
// flow (unpaid -> 402 -> pay -> 200). The receipt/transaction come back in the
// guarded response headers set by the seller SDK.
type BuyerCaller struct {
	Store registry.Store
	Buyer *buyer.Client
}

// Call implements Caller.
func (c *BuyerCaller) Call(ctx context.Context, serviceID string, args json.RawMessage) (CallResult, error) {
	svc, ok, err := c.Store.Get(ctx, serviceID)
	if err != nil {
		return CallResult{}, err
	}
	if !ok {
		return CallResult{}, fmt.Errorf("service not found: %s", serviceID)
	}

	resp, err := c.Buyer.Get(withArgs(svc.Endpoint, args))
	if err != nil {
		return CallResult{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return CallResult{}, err
	}

	return CallResult{
		Status:        resp.StatusCode,
		Body:          asJSON(body),
		ReceiptID:     resp.Header.Get("X-Tollgate-Receipt"),
		TransactionID: resp.Header.Get("X-Tollgate-Transaction"),
	}, nil
}

// withArgs appends flat arguments as query parameters for a GET invocation.
func withArgs(endpoint string, args json.RawMessage) string {
	if len(args) == 0 {
		return endpoint
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil || len(m) == 0 {
		return endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	q := u.Query()
	for k, v := range m {
		q.Set(k, fmt.Sprint(v))
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// asJSON returns body as raw JSON if it parses, otherwise a JSON string.
func asJSON(body []byte) json.RawMessage {
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	quoted, _ := json.Marshal(string(body))
	return quoted
}
