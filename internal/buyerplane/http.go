package buyerplane

import (
	"encoding/json"
	"net/http"

	"github.com/tollgate/tollgate/internal/policy"
	"github.com/tollgate/tollgate/x402"
)

// Server exposes the buyer plane over HTTP (docs/06-api-spec.md buyer plane).
type Server struct {
	plane *Plane
}

// NewServer wraps a Plane in an HTTP server.
func NewServer(p *Plane) *Server { return &Server{plane: p} }

// Routes returns the buyer-plane HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agents", s.handleCreateAgent)
	mux.HandleFunc("POST /v1/agents/{id}/wallet/fund", s.handleFund)
	mux.HandleFunc("GET /v1/agents/{id}/balance", s.handleBalance)
	mux.HandleFunc("POST /v1/agents/{id}/policies", s.handleCreatePolicy)
	mux.HandleFunc("GET /v1/agents/{id}/policies", s.handleGetPolicy)
	mux.HandleFunc("POST /v1/authorize", s.handleAuthorize)
	mux.HandleFunc("POST /v1/pay", s.handlePay)
	mux.HandleFunc("POST /v1/approvals/{id}/resolve", s.handleResolve)
	return mux
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	a, err := s.plane.CreateAgent(r.Context(), body.Label)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": a.ID, "wallet": a.Wallet, "label": a.Label})
}

func (s *Server) handleFund(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.plane.Fund(r.Context(), r.PathValue("id"), body.Amount, body.Currency); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"funded": true})
}

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	currency := r.URL.Query().Get("currency")
	if currency == "" {
		currency = "USDC"
	}
	bal, err := s.plane.Balance(r.Context(), r.PathValue("id"), currency)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"balance": bal, "currency": currency})
}

func (s *Server) handleCreatePolicy(w http.ResponseWriter, r *http.Request) {
	var pol policy.Policy
	if !decode(w, r, &pol) {
		return
	}
	created, err := s.plane.CreatePolicy(r.Context(), r.PathValue("id"), pol)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	pol, ok := s.plane.ActivePolicy(r.Context(), r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no active policy")
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeAuthorize(w, r)
	if !ok {
		return
	}
	dec, err := s.plane.Authorize(r.Context(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dec)
}

func (s *Server) handlePay(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AgentID           string     `json:"agentId"`
		TaskID            string     `json:"taskId"`
		Quote             x402.Quote `json:"quote"`
		ServiceCategory   string     `json:"serviceCategory"`
		MedianPrice       int64      `json:"medianPrice"`
		ApprovalRequestID string     `json:"approvalRequestId"`
	}
	if !decode(w, r, &body) {
		return
	}
	res, err := s.plane.Pay(r.Context(), PayInput{
		AuthorizeInput: AuthorizeInput{
			AgentID: body.AgentID, TaskID: body.TaskID, Quote: body.Quote,
			ServiceCategory: body.ServiceCategory, MedianPrice: body.MedianPrice,
		},
		ApprovalRequestID: body.ApprovalRequestID,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Approve bool `json:"approve"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	ar, ok := s.plane.ResolveApproval(r.Context(), r.PathValue("id"), body.Approve)
	if !ok {
		writeErr(w, http.StatusNotFound, "approval not found")
		return
	}
	writeJSON(w, http.StatusOK, ar)
}

func decodeAuthorize(w http.ResponseWriter, r *http.Request) (AuthorizeInput, bool) {
	var body struct {
		AgentID         string     `json:"agentId"`
		TaskID          string     `json:"taskId"`
		Quote           x402.Quote `json:"quote"`
		ServiceCategory string     `json:"serviceCategory"`
		MedianPrice     int64      `json:"medianPrice"`
	}
	if !decode(w, r, &body) {
		return AuthorizeInput{}, false
	}
	return AuthorizeInput{
		AgentID: body.AgentID, TaskID: body.TaskID, Quote: body.Quote,
		ServiceCategory: body.ServiceCategory, MedianPrice: body.MedianPrice,
	}, true
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
