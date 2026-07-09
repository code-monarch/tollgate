package marketplace

import (
	"bufio"
	"context"
	"encoding/json"
	"io"

	"github.com/tollgate/tollgate/internal/registry"
)

// ProtocolVersion is the MCP protocol revision this server speaks.
const ProtocolVersion = "2024-11-05"

// CallResult is what call_service returns: the invoked service's response plus
// the payment receipt reference from the completed 402 flow.
type CallResult struct {
	Status        int             `json:"status"`
	Body          json.RawMessage `json:"body,omitempty"`
	ReceiptID     string          `json:"receiptId,omitempty"`
	TransactionID string          `json:"transactionId,omitempty"`
}

// Caller runs the full 402 flow for a service (authorize -> pay -> invoke). It
// is injected so the MCP server stays decoupled from the buyer plane.
type Caller interface {
	Call(ctx context.Context, serviceID string, args json.RawMessage) (CallResult, error)
}

// MCP is the marketplace exposed as an MCP server: agents discover and call paid
// services the same way they use tools (docs/06-api-spec.md).
type MCP struct {
	store  registry.Store
	caller Caller // optional; call_service is unavailable if nil
}

// NewMCP builds an MCP server over a registry. caller may be nil to expose only
// discovery (search_services, get_service).
func NewMCP(store registry.Store, caller Caller) *MCP {
	return &MCP{store: store, caller: caller}
}

// --- JSON-RPC 2.0 envelope ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs the stdio transport: newline-delimited JSON-RPC 2.0 over r/w.
func (m *MCP) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		resp, ok := m.Handle(ctx, line)
		if !ok {
			continue // notification: no response
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Handle processes one JSON-RPC message and returns the response and whether one
// should be sent (false for notifications). Exported for testing without stdio.
func (m *MCP) Handle(ctx context.Context, raw []byte) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorResponse(nil, -32700, "parse error"), true
	}
	if len(req.ID) == 0 { // notification (e.g. notifications/initialized)
		return rpcResponse{}, false
	}

	switch req.Method {
	case "initialize":
		return okResponse(req.ID, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "tollgate-marketplace", "version": "0.1.0"},
		}), true
	case "ping":
		return okResponse(req.ID, map[string]any{}), true
	case "tools/list":
		return okResponse(req.ID, map[string]any{"tools": m.toolSpecs()}), true
	case "tools/call":
		return m.handleToolCall(ctx, req), true
	default:
		return errorResponse(req.ID, -32601, "method not found: "+req.Method), true
	}
}

func (m *MCP) handleToolCall(ctx context.Context, req rpcRequest) rpcResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errorResponse(req.ID, -32602, "invalid params")
	}
	switch params.Name {
	case "search_services":
		return m.toolSearch(ctx, req.ID, params.Arguments)
	case "get_service":
		return m.toolGet(ctx, req.ID, params.Arguments)
	case "call_service":
		return m.toolCall(ctx, req.ID, params.Arguments)
	default:
		return toolError(req.ID, "unknown tool: "+params.Name)
	}
}

func (m *MCP) toolSearch(ctx context.Context, id json.RawMessage, args json.RawMessage) rpcResponse {
	var a struct {
		Query         string   `json:"query"`
		Category      string   `json:"category"`
		MaxPrice      *int64   `json:"maxPrice"`
		MinReputation *float64 `json:"minReputation"`
	}
	_ = json.Unmarshal(args, &a)
	results, err := m.store.Search(ctx, registry.Query{
		Text: a.Query, Category: a.Category, MaxPrice: a.MaxPrice, MinReputation: a.MinReputation,
	})
	if err != nil {
		return toolError(id, err.Error())
	}
	if results == nil {
		results = []registry.Service{}
	}
	return toolJSON(id, map[string]any{"services": results})
}

func (m *MCP) toolGet(ctx context.Context, id json.RawMessage, args json.RawMessage) rpcResponse {
	var a struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(args, &a)
	svc, ok, err := m.store.Get(ctx, a.ID)
	if err != nil {
		return toolError(id, err.Error())
	}
	if !ok {
		return toolError(id, "service not found: "+a.ID)
	}
	return toolJSON(id, svc)
}

func (m *MCP) toolCall(ctx context.Context, id json.RawMessage, args json.RawMessage) rpcResponse {
	if m.caller == nil {
		return toolError(id, "call_service is not available on this server")
	}
	var a struct {
		ID   string          `json:"id"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return toolError(id, "invalid arguments")
	}
	res, err := m.caller.Call(ctx, a.ID, a.Args)
	if err != nil {
		return toolError(id, err.Error())
	}
	return toolJSON(id, res)
}

// toolSpecs advertises the three marketplace tools with JSON Schema inputs.
func (m *MCP) toolSpecs() []map[string]any {
	specs := []map[string]any{
		{
			"name":        "search_services",
			"description": "Search the Tollgate marketplace for paid services by text, category, max price (minor units) and minimum reputation.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":         map[string]any{"type": "string", "description": "free-text match on name/description/category"},
					"category":      map[string]any{"type": "string"},
					"maxPrice":      map[string]any{"type": "integer", "description": "max static price in minor units"},
					"minReputation": map[string]any{"type": "number", "description": "0..5"},
				},
			},
		},
		{
			"name":        "get_service",
			"description": "Fetch a service's full catalog entry (pricing, schema, SLA, reputation) by id.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string"}},
				"required":   []string{"id"},
			},
		},
	}
	if m.caller != nil {
		specs = append(specs, map[string]any{
			"name":        "call_service",
			"description": "Invoke a paid service by id, running the full x402 flow (authorize -> pay -> invoke) and returning the result plus a payment receipt.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":   map[string]any{"type": "string"},
					"args": map[string]any{"type": "object", "description": "request arguments for the service"},
				},
				"required": []string{"id"},
			},
		})
	}
	return specs
}

// --- helpers ---

func okResponse(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResponse(id json.RawMessage, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// toolJSON wraps a value as an MCP tool result (a single JSON text content).
func toolJSON(id json.RawMessage, v any) rpcResponse {
	text, err := json.Marshal(v)
	if err != nil {
		return toolError(id, err.Error())
	}
	return okResponse(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(text)}},
	})
}

// toolError returns a tool-level error (isError result, not a protocol error),
// per the MCP convention so the model can see and react to it.
func toolError(id json.RawMessage, msg string) rpcResponse {
	return okResponse(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	})
}

// toolResultText extracts the text of a tool result's first content block.
func toolResultText(resp rpcResponse) (string, bool) {
	result, ok := resp.Result.(map[string]any)
	if !ok {
		return "", false
	}
	content, ok := result["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		return "", false
	}
	text, ok := content[0]["text"].(string)
	return text, ok
}
