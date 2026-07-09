// Package marketplace exposes the registry over HTTP and as an MCP server, so
// agents discover paid services the same way they discover tools
// (docs/06-api-spec.md marketplace).
package marketplace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/tollgate/tollgate/internal/registry"
)

// Server serves the marketplace HTTP API over a registry Store.
type Server struct {
	store registry.Store
}

// NewServer wraps a registry Store.
func NewServer(store registry.Store) *Server { return &Server{store: store} }

// Routes returns the marketplace HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/services", s.handleRegister)
	mux.HandleFunc("GET /v1/services", s.handleSearch)
	mux.HandleFunc("GET /v1/services/{id}", s.handleGet)
	mux.HandleFunc("PUT /v1/services/{id}/pricing", s.handleSetPricing)
	return mux
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var svc registry.Service
	if err := json.NewDecoder(r.Body).Decode(&svc); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if svc.ID == "" {
		svc.ID = newID("svc")
	}
	if err := s.store.Put(r.Context(), svc); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	stored, _, _ := s.store.Get(r.Context(), svc.ID)
	writeJSON(w, http.StatusCreated, stored)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := registry.Query{
		Text:     r.URL.Query().Get("query"),
		Category: r.URL.Query().Get("category"),
	}
	if v := r.URL.Query().Get("maxPrice"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			q.MaxPrice = &n
		}
	}
	if v := r.URL.Query().Get("minReputation"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			q.MinReputation = &f
		}
	}
	results, err := s.store.Search(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if results == nil {
		results = []registry.Service{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": results})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	svc, ok, err := s.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "service not found")
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

func (s *Server) handleSetPricing(w http.ResponseWriter, r *http.Request) {
	var p registry.Pricing
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := s.store.SetPricing(r.Context(), r.PathValue("id"), p); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	svc, _, _ := s.store.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, http.StatusOK, svc)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("marketplace: crypto/rand failed: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
