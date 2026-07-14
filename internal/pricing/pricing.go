// Package pricing is the dynamic-pricing engine: it resolves the price to quote
// for a service at request time from a pricing model plus live demand signals.
// It is the seller-facing complement to internal/analytics — analytics tells a
// seller where to set the base price; this engine moves the actual quote around
// that base as demand shifts (docs/07-roadmap.md Milestone 5).
//
// Every resolution is a pure, bounded function of (model, signals): the same
// inputs always yield the same price and a plain-language rationale. No ML, no
// hidden state, no clock — deterministic so quotes are reproducible and auditable.
package pricing

import (
	"fmt"
	"math"
)

// Model configures how a service is priced. Type mirrors the registry's pricing
// model string ("static" | "variable" | "dynamic"); the tuning fields only apply
// to the demand-responsive models.
type Model struct {
	Type     string `json:"type"`
	Base     int64  `json:"base"`     // base price, minor units
	Currency string `json:"currency"` // e.g. "USDC"
	Floor    int64  `json:"floor"`    // lower bound; 0 ⇒ 1 (never free)
	Ceiling  int64  `json:"ceiling"`  // upper bound; 0 ⇒ unbounded

	// TargetRate is the demand the base price is tuned for — the calls-per-window
	// at which utilization is 1.0 (price == base). Only used by variable/dynamic.
	TargetRate float64 `json:"targetRate,omitempty"`
	// MaxSurge is the largest fractional move away from base, e.g. 0.5 ⇒ ±50%.
	MaxSurge float64 `json:"maxSurge,omitempty"`
}

// Signals are the live inputs to a resolution: how busy the service is right now.
type Signals struct {
	RecentCalls int64  `json:"recentCalls"` // settled calls in the current window
	Window      string `json:"window"`      // human label for the window, e.g. "1h"
}

// Resolved is a priced quote-input: the amount to charge plus why.
type Resolved struct {
	Price     int64   `json:"price"`
	Currency  string  `json:"currency"`
	Model     string  `json:"model"`
	Surge     float64 `json:"surge"` // fractional move applied vs base
	Rationale string  `json:"rationale"`
}

// Resolve prices the service under the current signals. An unknown or empty Type
// is treated as static (safest: charge exactly the base).
func (m Model) Resolve(s Signals) Resolved {
	switch m.Type {
	case "dynamic":
		return m.resolveDynamic(s)
	case "variable":
		return m.resolveVariable(s)
	default:
		return Resolved{
			Price: m.clamp(m.Base), Currency: m.Currency, Model: "static", Surge: 0,
			Rationale: fmt.Sprintf("static price %d %s", m.Base, m.Currency),
		}
	}
}

// resolveDynamic moves the price continuously with utilization: at target demand
// (util=1) it charges base; above target it surges up to +MaxSurge, below target
// it discounts down to -MaxSurge, linearly and symmetrically.
func (m Model) resolveDynamic(s Signals) Resolved {
	surge := m.surgeFromUtilization(s)
	price := m.clamp(int64(math.Round(float64(m.Base) * (1 + surge))))
	return Resolved{
		Price: price, Currency: m.Currency, Model: "dynamic", Surge: round2(surge),
		Rationale: fmt.Sprintf("dynamic: %d call(s)/%s vs target %.0f → %+.0f%% surge → %d %s",
			s.RecentCalls, windowLabel(s), m.TargetRate, surge*100, price, m.Currency),
	}
}

// resolveVariable is a coarser, banded cousin of dynamic: off-peak / normal / peak
// tiers instead of a continuous curve. Easier for sellers to reason about and to
// advertise ("2x at peak").
func (m Model) resolveVariable(s Signals) Resolved {
	util := m.utilization(s)
	var surge float64
	var tier string
	switch {
	case util < 0.5:
		surge, tier = -m.MaxSurge, "off-peak"
	case util <= 1.5:
		surge, tier = 0, "normal"
	default:
		surge, tier = m.MaxSurge, "peak"
	}
	price := m.clamp(int64(math.Round(float64(m.Base) * (1 + surge))))
	return Resolved{
		Price: price, Currency: m.Currency, Model: "variable", Surge: round2(surge),
		Rationale: fmt.Sprintf("variable: %s tier (%d call(s)/%s vs target %.0f) → %d %s",
			tier, s.RecentCalls, windowLabel(s), m.TargetRate, price, m.Currency),
	}
}

// utilization is demand relative to the target the base price was tuned for. With
// no target configured, utilization is 1.0 (neutral) so pricing collapses to base.
func (m Model) utilization(s Signals) float64 {
	if m.TargetRate <= 0 {
		return 1
	}
	return float64(s.RecentCalls) / m.TargetRate
}

// surgeFromUtilization maps utilization to a fractional price move in
// [-MaxSurge, +MaxSurge], linear around the neutral point util=1.
func (m Model) surgeFromUtilization(s Signals) float64 {
	if m.MaxSurge <= 0 {
		return 0
	}
	return m.MaxSurge * clamp(m.utilization(s)-1, -1, 1)
}

// clamp holds a resolved price inside [Floor, Ceiling], defaulting the floor to 1
// (a quote is never free) and leaving the ceiling open when unset.
func (m Model) clamp(p int64) int64 {
	floor := m.Floor
	if floor < 1 {
		floor = 1
	}
	if p < floor {
		p = floor
	}
	if m.Ceiling > 0 && p > m.Ceiling {
		p = m.Ceiling
	}
	return p
}

func windowLabel(s Signals) string {
	if s.Window == "" {
		return "window"
	}
	return s.Window
}

func clamp(f, lo, hi float64) float64 {
	if f < lo {
		return lo
	}
	if f > hi {
		return hi
	}
	return f
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
