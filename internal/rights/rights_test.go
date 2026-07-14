package rights

import (
	"reflect"
	"testing"

	"github.com/tollgate/tollgate/x402"
)

func TestEffective_DenyByDefault(t *testing.T) {
	offer := Offer{
		Required: []Right{Retain},
		Optional: []Right{Train, HumanReview},
	}

	// An empty grant grants nothing — silence never grants.
	if got := Effective(offer, nil); len(got) != 0 {
		t.Fatalf("empty grant yielded rights %v", got)
	}

	// Only what was both asked and granted crosses.
	got := Effective(offer, []Right{Retain, Train})
	if !reflect.DeepEqual(got, []Right{Retain, Train}) {
		t.Fatalf("effective = %v, want [retain train]", got)
	}
}

func TestEffective_CannotGrantMoreThanAsked(t *testing.T) {
	offer := Offer{Optional: []Right{Retain}}
	// The buyer over-grants; only the asked-for right crosses.
	got := Effective(offer, []Right{Retain, Train, Distill})
	if !reflect.DeepEqual(got, []Right{Retain}) {
		t.Fatalf("effective = %v, want only [retain]", got)
	}
}

func TestSatisfied_AndMissing(t *testing.T) {
	offer := Offer{Required: []Right{Train, Retain}, Optional: []Right{HumanReview}}

	if Satisfied(offer, []Right{Retain}) {
		t.Fatal("a partial grant satisfied a required set")
	}
	if got := Missing(offer, []Right{Retain}); !reflect.DeepEqual(got, []Right{Train}) {
		t.Fatalf("missing = %v, want [train]", got)
	}
	if !Satisfied(offer, []Right{Retain, Train}) {
		t.Fatal("a complete grant did not satisfy the required set")
	}
	// An offer requiring nothing is always satisfied, even by an empty grant.
	if !Satisfied(Offer{Optional: []Right{Train}}, nil) {
		t.Fatal("an offer with no required rights should always be satisfiable")
	}
}

func TestRebate_OnlyPaysForWhatCrossed(t *testing.T) {
	offer := Offer{
		Optional: []Right{Train, Retain},
		Rebates:  map[Right]int64{Train: 300, Retain: 50},
	}

	if got := Rebate(offer, Effective(offer, nil)); got != 0 {
		t.Fatalf("refusing everything earned %d, want 0", got)
	}
	if got := Rebate(offer, Effective(offer, []Right{Retain})); got != 50 {
		t.Fatalf("granting retain earned %d, want 50", got)
	}
	if got := Rebate(offer, Effective(offer, []Right{Train, Retain})); got != 350 {
		t.Fatalf("granting both earned %d, want 350", got)
	}
	// Granting something never asked for earns nothing.
	if got := Rebate(offer, Effective(offer, []Right{Distill})); got != 0 {
		t.Fatalf("granting an unasked right earned %d, want 0", got)
	}
}

func TestClamp_NeverNegativeNeverAbovePrice(t *testing.T) {
	if got := Clamp(1500, 1000); got != 1000 {
		t.Fatalf("clamp(1500, 1000) = %d, want 1000 (a call can be free, never negative)", got)
	}
	if got := Clamp(-5, 1000); got != 0 {
		t.Fatalf("clamp(-5, …) = %d, want 0", got)
	}
	if got := Clamp(200, 1000); got != 200 {
		t.Fatalf("clamp(200, 1000) = %d, want 200", got)
	}
}

func TestValidate(t *testing.T) {
	if err := (Offer{Required: []Right{"telepathy"}}).Validate(); err == nil {
		t.Fatal("unknown right accepted")
	}
	if err := (Offer{Optional: []Right{Train}, Rebates: map[Right]int64{Train: -1}}).Validate(); err == nil {
		t.Fatal("negative rebate accepted")
	}
	if err := (Offer{Optional: []Right{Train}, Rebates: map[Right]int64{Retain: 10}}).Validate(); err == nil {
		t.Fatal("rebate for an unasked right accepted")
	}
	if err := (Offer{Optional: []Right{Train}, Rebates: map[Right]int64{Train: 100}}).Validate(); err != nil {
		t.Fatalf("valid offer rejected: %v", err)
	}
}

func TestWireRoundTrip(t *testing.T) {
	offer := Offer{
		Required: []Right{Retain},
		Optional: []Right{Train},
		Rebates:  map[Right]int64{Train: 250},
	}
	got := FromWire(offer.ToWire())
	if !reflect.DeepEqual(Canonical(got.Required), []Right{Retain}) {
		t.Fatalf("required round trip = %v", got.Required)
	}
	if !reflect.DeepEqual(Canonical(got.Optional), []Right{Train}) {
		t.Fatalf("optional round trip = %v", got.Optional)
	}
	if got.Rebates[Train] != 250 {
		t.Fatalf("rebate round trip = %d, want 250", got.Rebates[Train])
	}

	// A seller claiming nothing encodes to nil — no exhaust section on the wire.
	if w := (Offer{}).ToWire(); w != nil {
		t.Fatalf("empty offer encoded to %+v, want nil", w)
	}
}

// An unknown right on the wire must never become a grantable right.
func TestFromWire_DropsUnknownRights(t *testing.T) {
	got := ParseRights([]string{"train", "mind_reading", "retain"})
	if !reflect.DeepEqual(got, []Right{Retain, Train}) {
		t.Fatalf("parsed = %v, want [retain train] with the unknown right dropped", got)
	}
}

// A malformed rebate must not become a reason to hand over rights: it reads as 0,
// and the rights decision itself is unaffected.
func TestFromWire_MalformedRebateIsZero(t *testing.T) {
	got := FromWire(&x402.ExhaustOffer{
		Optional: []string{"train"},
		Rebates:  map[string]string{"train": "not-a-number"},
	})
	if got.Rebates[Train] != 0 {
		t.Fatalf("malformed rebate parsed to %d, want 0", got.Rebates[Train])
	}
	if !reflect.DeepEqual(got.Optional, []Right{Train}) {
		t.Fatalf("a bad rebate corrupted the rights ask: %v", got.Optional)
	}
}
