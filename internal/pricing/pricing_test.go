package pricing

import "testing"

func TestStaticIgnoresSignals(t *testing.T) {
	m := Model{Type: "static", Base: 1000, Currency: "USDC"}
	for _, calls := range []int64{0, 5, 500} {
		got := m.Resolve(Signals{RecentCalls: calls})
		if got.Price != 1000 {
			t.Fatalf("static price moved to %d at %d calls", got.Price, calls)
		}
		if got.Surge != 0 {
			t.Fatalf("static surge = %v, want 0", got.Surge)
		}
	}
}

func TestUnknownTypeFallsBackToStatic(t *testing.T) {
	m := Model{Type: "", Base: 700, Currency: "USDC"}
	got := m.Resolve(Signals{RecentCalls: 999})
	if got.Model != "static" || got.Price != 700 {
		t.Fatalf("empty type => %s @ %d, want static @ 700", got.Model, got.Price)
	}
}

func TestDynamicSurgesAndDiscountsSymmetrically(t *testing.T) {
	m := Model{Type: "dynamic", Base: 1000, Currency: "USDC", TargetRate: 10, MaxSurge: 0.5}

	// At target: no move.
	if got := m.Resolve(Signals{RecentCalls: 10}); got.Price != 1000 {
		t.Fatalf("at target price = %d, want 1000", got.Price)
	}
	// Double the target: full +50% surge (utilization-1 clamps at 1).
	if got := m.Resolve(Signals{RecentCalls: 20}); got.Price != 1500 {
		t.Fatalf("at 2x target price = %d, want 1500", got.Price)
	}
	// Way above target still caps at +MaxSurge.
	if got := m.Resolve(Signals{RecentCalls: 1000}); got.Price != 1500 {
		t.Fatalf("far above target price = %d, want capped 1500", got.Price)
	}
	// Zero demand: full -50% discount.
	if got := m.Resolve(Signals{RecentCalls: 0}); got.Price != 500 {
		t.Fatalf("no demand price = %d, want 500", got.Price)
	}
	// Half target: linear -25%.
	if got := m.Resolve(Signals{RecentCalls: 5}); got.Price != 750 {
		t.Fatalf("half target price = %d, want 750", got.Price)
	}
}

func TestDynamicRespectsFloorAndCeiling(t *testing.T) {
	m := Model{Type: "dynamic", Base: 1000, Currency: "USDC", TargetRate: 10, MaxSurge: 0.9, Floor: 400, Ceiling: 1500}
	if got := m.Resolve(Signals{RecentCalls: 0}); got.Price != 400 { // -90% => 100, floored to 400
		t.Fatalf("floored price = %d, want 400", got.Price)
	}
	if got := m.Resolve(Signals{RecentCalls: 100}); got.Price != 1500 { // +90% => 1900, capped 1500
		t.Fatalf("ceiled price = %d, want 1500", got.Price)
	}
}

func TestFloorNeverBelowOne(t *testing.T) {
	m := Model{Type: "dynamic", Base: 2, Currency: "USDC", TargetRate: 10, MaxSurge: 1.0}
	if got := m.Resolve(Signals{RecentCalls: 0}); got.Price != 1 { // -100% => 0, floored to 1
		t.Fatalf("price = %d, want 1 (never free)", got.Price)
	}
}

func TestVariableTiers(t *testing.T) {
	m := Model{Type: "variable", Base: 1000, Currency: "USDC", TargetRate: 10, MaxSurge: 0.2}
	cases := []struct {
		calls int64
		want  int64
	}{
		{2, 800},   // util 0.2 < 0.5 => off-peak -20%
		{10, 1000}, // util 1.0 => normal
		{14, 1000}, // util 1.4 <= 1.5 => normal
		{30, 1200}, // util 3.0 > 1.5 => peak +20%
	}
	for _, c := range cases {
		if got := m.Resolve(Signals{RecentCalls: c.calls}); got.Price != c.want {
			t.Fatalf("variable @ %d calls = %d, want %d", c.calls, got.Price, c.want)
		}
	}
}

func TestNoTargetIsNeutral(t *testing.T) {
	m := Model{Type: "dynamic", Base: 1000, Currency: "USDC", MaxSurge: 0.5} // TargetRate 0
	if got := m.Resolve(Signals{RecentCalls: 999}); got.Price != 1000 {
		t.Fatalf("no target price = %d, want base 1000", got.Price)
	}
}

func TestDeterministic(t *testing.T) {
	m := Model{Type: "dynamic", Base: 1234, Currency: "USDC", TargetRate: 7, MaxSurge: 0.4}
	s := Signals{RecentCalls: 11, Window: "1h"}
	first := m.Resolve(s)
	for i := 0; i < 100; i++ {
		if m.Resolve(s) != first {
			t.Fatal("resolution is not deterministic")
		}
	}
}
