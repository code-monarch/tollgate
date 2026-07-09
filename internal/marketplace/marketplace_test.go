package marketplace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tollgate/tollgate/internal/registry"
)

func seededStore(t *testing.T) *registry.MemStore {
	t.Helper()
	m := registry.NewMemStore()
	must(t, m.Put(context.Background(), registry.Service{
		ID: "svc_geo", Name: "Geocoder", Description: "address to lat/lng", Category: "geo",
		Endpoint: "https://api.example.com/geocode",
		Pricing:  registry.Pricing{Model: "static", Amount: "1000", Currency: "USDC"},
		SLA:      registry.SLA{Uptime: 0.999},
	}))
	return m
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// --- HTTP API ---

func TestHTTP_RegisterSearchGet(t *testing.T) {
	srv := httptest.NewServer(NewServer(registry.NewMemStore()).Routes())
	defer srv.Close()

	body := `{"name":"Geocoder","description":"geo","category":"geo",
	          "endpoint":"https://x/geocode","sellerWallet":"wallet:s",
	          "pricing":{"model":"static","amount":"1000","currency":"USDC"},
	          "sla":{"uptime":0.99}}`
	resp, err := http.Post(srv.URL+"/v1/services", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created registry.Service
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.ID == "" {
		t.Fatalf("register status=%d id=%q", resp.StatusCode, created.ID)
	}

	// Search finds it.
	sr, err := http.Get(srv.URL + "/v1/services?query=geo")
	if err != nil {
		t.Fatal(err)
	}
	var searchResp struct {
		Services []registry.Service `json:"services"`
	}
	json.NewDecoder(sr.Body).Decode(&searchResp)
	sr.Body.Close()
	if len(searchResp.Services) != 1 {
		t.Fatalf("search returned %d", len(searchResp.Services))
	}

	// Get by id.
	gr, err := http.Get(srv.URL + "/v1/services/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gr.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", gr.StatusCode)
	}
	gr.Body.Close()

	// Update pricing.
	pr, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/services/"+created.ID+"/pricing",
		strings.NewReader(`{"model":"static","amount":"2000","currency":"USDC"}`))
	presp, err := http.DefaultClient.Do(pr)
	if err != nil {
		t.Fatal(err)
	}
	var updated registry.Service
	json.NewDecoder(presp.Body).Decode(&updated)
	presp.Body.Close()
	if updated.Pricing.Amount != "2000" {
		t.Fatalf("pricing not updated: %+v", updated.Pricing)
	}
}

func TestHTTP_GetMissing(t *testing.T) {
	srv := httptest.NewServer(NewServer(registry.NewMemStore()).Routes())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/services/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing service status = %d, want 404", resp.StatusCode)
	}
}

// --- MCP ---

type stubCaller struct {
	called string
}

func (s *stubCaller) Call(_ context.Context, serviceID string, _ json.RawMessage) (CallResult, error) {
	s.called = serviceID
	return CallResult{Status: 200, Body: json.RawMessage(`{"lat":37.42}`), ReceiptID: "rcpt-1", TransactionID: "txn-1"}, nil
}

func handle(t *testing.T, m *MCP, req string) rpcResponse {
	t.Helper()
	resp, ok := m.Handle(context.Background(), []byte(req))
	if !ok {
		t.Fatal("expected a response")
	}
	return resp
}

func TestMCP_InitializeAndToolsList(t *testing.T) {
	m := NewMCP(seededStore(t), &stubCaller{})

	init := handle(t, m, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if init.Error != nil {
		t.Fatalf("initialize error: %v", init.Error)
	}
	res := init.Result.(map[string]any)
	if res["protocolVersion"] != ProtocolVersion {
		t.Fatalf("protocolVersion = %v", res["protocolVersion"])
	}

	list := handle(t, m, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := list.Result.(map[string]any)["tools"].([]map[string]any)
	if len(tools) != 3 {
		t.Fatalf("tools = %d, want 3 (search/get/call)", len(tools))
	}
}

func TestMCP_NotificationHasNoResponse(t *testing.T) {
	m := NewMCP(seededStore(t), nil)
	if _, ok := m.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); ok {
		t.Fatal("notification should not produce a response")
	}
}

func TestMCP_SearchAndGetTools(t *testing.T) {
	m := NewMCP(seededStore(t), nil)

	resp := handle(t, m, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_services","arguments":{"query":"geo"}}}`)
	text, ok := toolResultText(resp)
	if !ok || !strings.Contains(text, "svc_geo") {
		t.Fatalf("search tool result = %q", text)
	}

	resp = handle(t, m, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_service","arguments":{"id":"svc_geo"}}}`)
	text, _ = toolResultText(resp)
	if !strings.Contains(text, "Geocoder") {
		t.Fatalf("get tool result = %q", text)
	}
}

func TestMCP_CallServiceInvokesCaller(t *testing.T) {
	caller := &stubCaller{}
	m := NewMCP(seededStore(t), caller)

	resp := handle(t, m, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"call_service","arguments":{"id":"svc_geo","args":{"q":"hi"}}}}`)
	text, _ := toolResultText(resp)
	if caller.called != "svc_geo" {
		t.Fatalf("caller invoked with %q", caller.called)
	}
	if !strings.Contains(text, "rcpt-1") {
		t.Fatalf("call result missing receipt: %q", text)
	}
}

func TestMCP_CallServiceUnavailableWithoutCaller(t *testing.T) {
	m := NewMCP(seededStore(t), nil)
	// call_service should not even be advertised.
	list := handle(t, m, `{"jsonrpc":"2.0","id":6,"method":"tools/list"}`)
	tools := list.Result.(map[string]any)["tools"].([]map[string]any)
	if len(tools) != 2 {
		t.Fatalf("tools without caller = %d, want 2", len(tools))
	}
	resp := handle(t, m, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"call_service","arguments":{"id":"svc_geo"}}}`)
	if text, _ := toolResultText(resp); !strings.Contains(text, "not available") {
		t.Fatalf("expected unavailable message, got %q", text)
	}
}

func TestMCP_UnknownMethod(t *testing.T) {
	m := NewMCP(seededStore(t), nil)
	resp := handle(t, m, `{"jsonrpc":"2.0","id":8,"method":"bogus"}`)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("expected method-not-found, got %+v", resp.Error)
	}
}
