// Package registry is the marketplace catalog: the machine-readable list of
// paid endpoints agents discover and price at runtime. It holds richer service
// entries than the facilitator needs for quoting (schema, SLA, category,
// reputation) and supports search by query/price/category/reputation
// (docs/03-data-model.md `services`, docs/06-api-spec.md marketplace).
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Pricing describes how a service is priced. Milestone 1 pricing is static;
// variable/dynamic are recorded for discovery but resolved at quote time.
type Pricing struct {
	Model    string `json:"model"`    // "static" | "variable" | "dynamic"
	Amount   string `json:"amount"`   // minor units (static)
	Currency string `json:"currency"` // e.g. "USDC"
}

// SLA is a service's advertised service level.
type SLA struct {
	Uptime    float64 `json:"uptime"`    // 0..1
	LatencyMs int     `json:"latencyMs"` // typical latency claim
}

// Service is a marketplace catalog entry.
type Service struct {
	ID           string          `json:"id"`
	SellerID     string          `json:"sellerId"`
	SellerWallet string          `json:"sellerWallet"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Endpoint     string          `json:"endpoint"`
	Category     string          `json:"category"`
	Pricing      Pricing         `json:"pricing"`
	Schema       json.RawMessage `json:"schema,omitempty"`
	SLA          SLA             `json:"sla"`
	Status       string          `json:"status"` // "listed" | "unlisted"
	Reputation   float64         `json:"reputation"`
	CreatedAt    time.Time       `json:"createdAt"`
}

// Query filters a Search. Zero-valued fields are ignored.
type Query struct {
	Text          string // substring match on name/description/category
	Category      string // exact category
	MaxPrice      *int64 // max static price in minor units
	MinReputation *float64
}

// Store is the catalog persistence contract. In-memory for Milestone 3; the
// db/schema.sql `services` table is the eventual home.
type Store interface {
	Put(ctx context.Context, s Service) error
	Get(ctx context.Context, id string) (Service, bool, error)
	Search(ctx context.Context, q Query) ([]Service, error)
	SetPricing(ctx context.Context, id string, p Pricing) error
}

// MemStore is an in-memory Store with a reputation model.
type MemStore struct {
	mu    sync.Mutex
	byID  map[string]Service
	stats map[string]*stats // reputation signals per service
	now   func() time.Time
}

// NewMemStore returns an empty catalog.
func NewMemStore() *MemStore {
	return &MemStore{
		byID:  make(map[string]Service),
		stats: make(map[string]*stats),
		now:   time.Now,
	}
}

// Put inserts or replaces a service, defaulting status and reputation.
func (m *MemStore) Put(ctx context.Context, s Service) error {
	if s.ID == "" {
		return fmt.Errorf("registry: service id required")
	}
	if s.Status == "" {
		s.Status = "listed"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = m.now().UTC()
	}
	if _, ok := m.stats[s.ID]; !ok {
		m.stats[s.ID] = &stats{}
	}
	s.Reputation = m.stats[s.ID].score(s.SLA)
	m.byID[s.ID] = s
	return nil
}

// Get returns a service by id.
func (m *MemStore) Get(ctx context.Context, id string) (Service, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	return s, ok, nil
}

// SetPricing updates a service's pricing model.
func (m *MemStore) SetPricing(ctx context.Context, id string, p Pricing) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("registry: unknown service %q", id)
	}
	s.Pricing = p
	m.byID[id] = s
	return nil
}

// Search returns listed services matching q, ranked by reputation (desc).
func (m *MemStore) Search(ctx context.Context, q Query) ([]Service, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Service
	for _, s := range m.byID {
		if s.Status != "listed" {
			continue
		}
		if !matches(s, q) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reputation != out[j].Reputation {
			return out[i].Reputation > out[j].Reputation
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func matches(s Service, q Query) bool {
	if q.Text != "" {
		hay := strings.ToLower(s.Name + " " + s.Description + " " + s.Category)
		if !strings.Contains(hay, strings.ToLower(q.Text)) {
			return false
		}
	}
	if q.Category != "" && !strings.EqualFold(s.Category, q.Category) {
		return false
	}
	if q.MinReputation != nil && s.Reputation < *q.MinReputation {
		return false
	}
	if q.MaxPrice != nil && s.Pricing.Model == "static" {
		amt, err := parseMinor(s.Pricing.Amount)
		if err != nil || amt > *q.MaxPrice {
			return false
		}
	}
	return true
}
