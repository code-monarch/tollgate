package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/pricing"
	"github.com/tollgate/tollgate/internal/registry"
)

// Server exposes the seller analytics + dynamic-pricing endpoints over HTTP
// (docs/06-api-spec.md GET /v1/analytics/services/{id}). It reads the ledger and
// the catalog; it never writes either.
type Server struct {
	ledger   ledger.Store
	catalog  registry.Store
	window   time.Duration    // demand window for pricing resolution (default 1h)
	lookback time.Duration    // default analytics window when none is requested
	now      func() time.Time // injectable clock (tests)
}

// Option customizes a Server.
type Option func(*Server)

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithWindow sets the demand window used when resolving a dynamic price
// (default 1h) — recent calls inside this window drive the surge.
func WithWindow(d time.Duration) Option { return func(s *Server) { s.window = d } }

// WithLookback sets the default analytics window when a request omits `since`
// (default 30 days).
func WithLookback(d time.Duration) Option { return func(s *Server) { s.lookback = d } }

// NewServer builds an analytics server over a ledger and catalog.
func NewServer(l ledger.Store, catalog registry.Store, opts ...Option) *Server {
	s := &Server{
		ledger:   l,
		catalog:  catalog,
		window:   time.Hour,
		lookback: 30 * 24 * time.Hour,
		now:      time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Routes returns the analytics + pricing HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/analytics/services/{id}", s.handleReport)
	mux.HandleFunc("GET /v1/pricing/services/{id}", s.handleResolvePrice)
	return mux
}

// handleReport returns revenue/cohorts/elasticity/recommendation for a service.
// `since` is optional: an RFC3339 timestamp or a Go duration ("24h", "7d"→use
// hours); it defaults to the configured lookback.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	since := s.now().Add(-s.lookback).UTC()
	if v := r.URL.Query().Get("since"); v != "" {
		parsed, err := s.parseSince(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid since: "+err.Error())
			return
		}
		since = parsed
	}
	rep, err := Service(r.Context(), s.ledger, s.catalog, r.PathValue("id"), since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleResolvePrice resolves the price to quote for a service right now, from its
// pricing model and the settled-call count in the demand window.
func (s *Server) handleResolvePrice(w http.ResponseWriter, r *http.Request) {
	window := s.window
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid window: "+err.Error())
			return
		}
		window = d
	}

	svc, ok, err := s.catalog.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "service not found")
		return
	}

	model, merr := pricingModel(svc.Pricing)
	if merr != nil {
		writeErr(w, http.StatusUnprocessableEntity, merr.Error())
		return
	}
	recent, err := s.recentCalls(r.Context(), svc.ID, window)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resolved := model.Resolve(pricing.Signals{RecentCalls: recent, Window: window.String()})
	writeJSON(w, http.StatusOK, resolved)
}

// recentCalls counts settled transactions for a service within the trailing
// window — the demand signal the pricing engine surges on.
func (s *Server) recentCalls(ctx context.Context, serviceID string, window time.Duration) (int64, error) {
	txns, err := s.ledger.Transactions(ctx, ledger.TxQuery{
		ServiceID: serviceID,
		Since:     s.now().Add(-window).UTC(),
		Statuses:  []ledger.Status{ledger.StatusSettled},
	})
	if err != nil {
		return 0, err
	}
	return int64(len(txns)), nil
}

func (s *Server) parseSince(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return time.Time{}, err
	}
	return s.now().Add(-d).UTC(), nil
}

// pricingModel maps a catalog pricing entry onto the pricing engine's model. The
// base amount is parsed from the (string, minor-unit) Amount field.
func pricingModel(p registry.Pricing) (pricing.Model, error) {
	base, err := strconv.ParseInt(p.Amount, 10, 64)
	if err != nil {
		return pricing.Model{}, err
	}
	model := p.Model
	if model == "" {
		model = "static"
	}
	return pricing.Model{
		Type: model, Base: base, Currency: p.Currency,
		Floor: p.Floor, Ceiling: p.Ceiling, TargetRate: p.TargetRate, MaxSurge: p.MaxSurge,
	}, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
