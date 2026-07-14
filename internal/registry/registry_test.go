package registry

import (
	"context"
	"testing"

	"github.com/tollgate/tollgate/internal/rights"
)

func ptrI(v int64) *int64     { return &v }
func ptrF(v float64) *float64 { return &v }

func seed(t *testing.T) *MemStore {
	t.Helper()
	m := NewMemStore()
	ctx := context.Background()
	must(t, m.Put(ctx, Service{
		ID: "svc_geo", Name: "Geocoder", Description: "address to lat/lng",
		Category: "geo", Endpoint: "https://api.example.com/geocode",
		Pricing: Pricing{Model: "static", Amount: "1000", Currency: "USDC"},
		SLA:     SLA{Uptime: 0.999},
	}))
	must(t, m.Put(ctx, Service{
		ID: "svc_img", Name: "Image Gen", Description: "text to image",
		Category: "ml", Endpoint: "https://api.example.com/img",
		Pricing: Pricing{Model: "variable", Currency: "USDC"},
		SLA:     SLA{Uptime: 0.95},
	}))
	must(t, m.Put(ctx, Service{
		ID: "svc_hidden", Name: "Hidden", Category: "geo", Status: "unlisted",
		Pricing: Pricing{Model: "static", Amount: "500", Currency: "USDC"},
		SLA:     SLA{Uptime: 1.0},
	}))
	return m
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSearch_TextAndCategory(t *testing.T) {
	m := seed(t)
	ctx := context.Background()

	got, _ := m.Search(ctx, Query{Text: "image"})
	if len(got) != 1 || got[0].ID != "svc_img" {
		t.Fatalf("text search = %+v", ids(got))
	}
	got, _ = m.Search(ctx, Query{Category: "geo"})
	if len(got) != 1 || got[0].ID != "svc_geo" { // unlisted excluded
		t.Fatalf("category search = %v", ids(got))
	}
}

func TestSearch_MaxPriceAndReputation(t *testing.T) {
	m := seed(t)
	ctx := context.Background()

	// Only static-priced services are bounded by MaxPrice; svc_geo is 1000.
	if got, _ := m.Search(ctx, Query{MaxPrice: ptrI(999)}); len(ids(got)) != 1 || got[0].ID != "svc_img" {
		t.Fatalf("maxPrice 999 = %v (variable pricing should pass, static 1000 should not)", ids(got))
	}
	if got, _ := m.Search(ctx, Query{MaxPrice: ptrI(1000)}); len(got) != 2 {
		t.Fatalf("maxPrice 1000 = %v, want both", ids(got))
	}
	// svc_geo seeds reputation from uptime 0.999 -> ~5.0; svc_img 0.95 -> ~4.75.
	if got, _ := m.Search(ctx, Query{MinReputation: ptrF(4.9)}); len(got) != 1 || got[0].ID != "svc_geo" {
		t.Fatalf("minReputation filter = %v", ids(got))
	}
}

func TestSearch_RankedByReputation(t *testing.T) {
	m := seed(t)
	got, _ := m.Search(context.Background(), Query{})
	if len(got) != 2 || got[0].ID != "svc_geo" {
		t.Fatalf("ranking = %v, want svc_geo first", ids(got))
	}
}

func TestReputation_DropsWithDisputes(t *testing.T) {
	m := seed(t)
	ctx := context.Background()
	before, _, _ := m.Get(ctx, "svc_geo")

	// 10 calls, 5 disputed → dispute rate 0.5.
	for i := 0; i < 10; i++ {
		must(t, m.RecordOutcome(ctx, "svc_geo", i < 5, false))
	}
	after, _, _ := m.Get(ctx, "svc_geo")
	if after.Reputation >= before.Reputation {
		t.Fatalf("reputation did not drop: %v -> %v", before.Reputation, after.Reputation)
	}
	// 0.7*(1-0.5) + 0.3*1 = 0.65 → 3.25
	if after.Reputation != 3.25 {
		t.Fatalf("reputation = %v, want 3.25", after.Reputation)
	}
}

func TestSetPricing(t *testing.T) {
	m := seed(t)
	ctx := context.Background()
	must(t, m.SetPricing(ctx, "svc_geo", Pricing{Model: "static", Amount: "2000", Currency: "USDC"}))
	s, _, _ := m.Get(ctx, "svc_geo")
	if s.Pricing.Amount != "2000" {
		t.Fatalf("pricing not updated: %+v", s.Pricing)
	}
	if err := m.SetPricing(ctx, "nope", Pricing{}); err == nil {
		t.Fatal("SetPricing on unknown service should error")
	}
}

func ids(ss []Service) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

// A firm shops its own boundary: find a service that will not demand the right to
// train on its queries. This is a search, not a legal review
// (docs/08-learning-boundary.md).
func TestSearch_ExcludeRequiredRights(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()

	// A model that will not serve unless it may train on you.
	mustPut(t, s, Service{
		ID: "svc_grabby", Name: "Grabby", Category: "nlp",
		Pricing: Pricing{Model: "static", Amount: "100", Currency: "USDC"},
		SLA:     SLA{Uptime: 0.99},
		Exhaust: rights.Offer{Required: []rights.Right{rights.Train}},
	})
	// A model that merely ASKS to train, and pays for it — still a usable choice,
	// because the buyer can decline the ask and pay list price.
	mustPut(t, s, Service{
		ID: "svc_polite", Name: "Polite", Category: "nlp",
		Pricing: Pricing{Model: "static", Amount: "100", Currency: "USDC"},
		SLA:     SLA{Uptime: 0.99},
		Exhaust: rights.Offer{
			Optional: []rights.Right{rights.Train},
			Rebates:  map[rights.Right]int64{rights.Train: 30},
		},
	})
	// A model that claims nothing at all.
	mustPut(t, s, Service{
		ID: "svc_clean", Name: "Clean", Category: "nlp",
		Pricing: Pricing{Model: "static", Amount: "100", Currency: "USDC"},
		SLA:     SLA{Uptime: 0.99},
	})

	all, err := s.Search(ctx, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered search returned %d, want 3", len(all))
	}

	got, err := s.Search(ctx, Query{ExcludeRequired: []rights.Right{rights.Train}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("excludeRequired=train returned %d services, want 2", len(got))
	}
	for _, svc := range got {
		if svc.ID == "svc_grabby" {
			t.Fatal("a service that REQUIRES training rights survived the filter")
		}
	}
}

func mustPut(t *testing.T, s *MemStore, svc Service) {
	t.Helper()
	if err := s.Put(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
}
