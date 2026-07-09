package registry

import (
	"context"
	"testing"
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
