// Package analytics turns the append-only ledger into seller-facing metrics:
// revenue per route, caller cohorts, price elasticity, and a revenue-maximizing
// price recommendation. It is the retention layer — the thing that makes sellers
// stay (docs/02-architecture.md analytics, docs/07-roadmap.md Milestone 5).
//
// Everything here is pure, deterministic and explainable — no ML on any path,
// matching the reputation model's ethos (internal/registry/reputation.go). The
// only source of truth is settled ledger transactions; there is no separate
// metrics store to drift out of sync.
package analytics

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/registry"
)

// Window is the closed time range a report covers.
type Window struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// PricePoint is the demand observed at one price: how many settled calls landed
// at that price and the revenue they produced. It is the raw material for the
// elasticity estimate.
type PricePoint struct {
	Price   int64 `json:"price"`   // minor units
	Calls   int64 `json:"calls"`   // settled calls at this price
	Revenue int64 `json:"revenue"` // Price * Calls
}

// Cohort is one caller's footprint on a service over the window.
type Cohort struct {
	AgentID string `json:"agentId"`
	Calls   int64  `json:"calls"`
	Spend   int64  `json:"spend"` // gross, minor units
	// DividendEarned is what this caller was paid back for the exhaust rights it
	// granted — what its knowledge was worth to the seller.
	DividendEarned int64 `json:"dividendEarned"`
}

// Elasticity is the estimated price elasticity of demand — the percent change in
// quantity for a percent change in price. It is fitted by ordinary least squares
// on log(quantity) vs log(price) across the observed price points, so it is a
// cross-sectional proxy, not a controlled experiment. Coefficient is typically
// negative (raise price, sell less).
type Elasticity struct {
	Coefficient float64 `json:"coefficient"`
	Points      int     `json:"points"` // distinct price points used in the fit
	Method      string  `json:"method"` // "log-log-ols" | "insufficient-variation"
	Explanation string  `json:"explanation"`
}

// Estimated reports whether the fit had enough price variation to be usable.
func (e Elasticity) Estimated() bool { return e.Method == "log-log-ols" }

// Recommendation is a price move derived from the elasticity estimate, bounded to
// a conservative step so a single noisy window can't swing the price wildly.
type Recommendation struct {
	CurrentPrice     int64   `json:"currentPrice"`
	RecommendedPrice int64   `json:"recommendedPrice"`
	Currency         string  `json:"currency"`
	ExpectedLift     float64 `json:"expectedRevenueLift"` // fractional, e.g. 0.06 = +6%
	Rationale        string  `json:"rationale"`
}

// Report is the full analytics view of one service over a window.
type Report struct {
	ServiceID     string `json:"serviceId"`
	Window        Window `json:"window"`
	Currency      string `json:"currency"`
	Revenue       int64  `json:"revenue"` // gross, minor units, settled only
	Calls         int64  `json:"calls"`
	UniqueCallers int    `json:"uniqueCallers"`
	AvgPrice      int64  `json:"avgPrice"`

	// DividendPaid is what this service paid its callers for the exhaust rights they
	// granted, and NetRevenue is what it actually kept (Revenue - DividendPaid). A
	// seller that learns from its customers does not get to book that knowledge as
	// free: the cost of it shows up here (docs/08-learning-boundary.md).
	DividendPaid int64 `json:"dividendPaid"`
	NetRevenue   int64 `json:"netRevenue"`

	PricePoints    []PricePoint   `json:"pricePoints"`
	Cohorts        []Cohort       `json:"cohorts"`
	Elasticity     Elasticity     `json:"elasticity"`
	Recommendation Recommendation `json:"recommendation"`
}

// maxStep bounds a single pricing recommendation to ±15% of the current price.
// Deterministic and conservative: elasticity fitted on one window is noisy, so we
// nudge rather than leap and re-measure next window.
const maxStep = 0.15

// unitBand is the half-width around unit elasticity (coefficient == -1) inside
// which revenue is flat w.r.t. price, so the recommendation is to hold.
const unitBand = 0.15

// Service computes the report for one service from settled ledger transactions at
// or after since, pricing the recommendation against the service's current
// advertised price. It only reads — it never writes the ledger or the catalog.
func Service(ctx context.Context, l ledger.Store, r registry.Store, serviceID string, since time.Time) (Report, error) {
	txns, err := l.Transactions(ctx, ledger.TxQuery{
		ServiceID: serviceID,
		Since:     since,
		Statuses:  []ledger.Status{ledger.StatusSettled},
	})
	if err != nil {
		return Report{}, err
	}

	var currentPrice int64
	var currency string
	if svc, ok, err := r.Get(ctx, serviceID); err != nil {
		return Report{}, err
	} else if ok {
		currency = svc.Pricing.Currency
		if svc.Pricing.Model == "static" {
			if amt, perr := strconv.ParseInt(svc.Pricing.Amount, 10, 64); perr == nil {
				currentPrice = amt
			}
		}
	}
	return Compute(serviceID, txns, currentPrice, currency), nil
}

// Compute builds a report from an already-fetched set of settled transactions. It
// is pure so it can be unit-tested and reused off a snapshot. currentPrice may be
// 0 (unknown) — the recommendation then anchors on the modal observed price.
func Compute(serviceID string, txns []ledger.Transaction, currentPrice int64, currency string) Report {
	rep := Report{ServiceID: serviceID, Currency: currency}
	if len(txns) == 0 {
		rep.Recommendation = holdRecommendation(currentPrice, currency,
			"no settled calls in the window yet — nothing to price against")
		return rep
	}

	byPrice := map[int64]*PricePoint{}
	byAgent := map[string]*Cohort{}
	var from, to time.Time
	for i, tx := range txns {
		if currency == "" {
			currency = tx.Currency
		}
		if i == 0 || tx.CreatedAt.Before(from) {
			from = tx.CreatedAt
		}
		if i == 0 || tx.CreatedAt.After(to) {
			to = tx.CreatedAt
		}
		rep.Revenue += tx.Amount
		rep.DividendPaid += tx.Rebate
		rep.Calls++

		pp := byPrice[tx.Amount]
		if pp == nil {
			pp = &PricePoint{Price: tx.Amount}
			byPrice[tx.Amount] = pp
		}
		pp.Calls++
		pp.Revenue += tx.Amount

		co := byAgent[tx.AgentID]
		if co == nil {
			co = &Cohort{AgentID: tx.AgentID}
			byAgent[tx.AgentID] = co
		}
		co.Calls++
		co.Spend += tx.Amount
		co.DividendEarned += tx.Rebate
	}

	rep.NetRevenue = rep.Revenue - rep.DividendPaid
	rep.Currency = currency
	rep.Window = Window{From: from, To: to}
	rep.UniqueCallers = len(byAgent)
	rep.AvgPrice = rep.Revenue / rep.Calls

	rep.PricePoints = make([]PricePoint, 0, len(byPrice))
	for _, pp := range byPrice {
		rep.PricePoints = append(rep.PricePoints, *pp)
	}
	sort.Slice(rep.PricePoints, func(i, j int) bool { return rep.PricePoints[i].Price < rep.PricePoints[j].Price })

	rep.Cohorts = make([]Cohort, 0, len(byAgent))
	for _, co := range byAgent {
		rep.Cohorts = append(rep.Cohorts, *co)
	}
	sort.Slice(rep.Cohorts, func(i, j int) bool {
		if rep.Cohorts[i].Spend != rep.Cohorts[j].Spend {
			return rep.Cohorts[i].Spend > rep.Cohorts[j].Spend
		}
		return rep.Cohorts[i].AgentID < rep.Cohorts[j].AgentID
	})

	rep.Elasticity = estimateElasticity(rep.PricePoints)

	if currentPrice <= 0 {
		currentPrice = modalPrice(rep.PricePoints)
	}
	rep.Recommendation = recommend(currentPrice, currency, rep.Elasticity)
	return rep
}

// estimateElasticity fits log(quantity) = a + e*log(price) by ordinary least
// squares; the slope e is the price elasticity of demand. It needs at least two
// distinct prices with non-zero demand, otherwise the slope is undefined.
func estimateElasticity(points []PricePoint) Elasticity {
	var xs, ys []float64
	for _, p := range points {
		if p.Price <= 0 || p.Calls <= 0 {
			continue
		}
		xs = append(xs, math.Log(float64(p.Price)))
		ys = append(ys, math.Log(float64(p.Calls)))
	}
	if len(xs) < 2 {
		return Elasticity{
			Method:      "insufficient-variation",
			Points:      len(xs),
			Explanation: "need at least two distinct price points with demand to estimate elasticity; vary the price to measure it",
		}
	}

	n := float64(len(xs))
	var sx, sy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
	}
	mx, my := sx/n, sy/n
	var num, den float64
	for i := range xs {
		dx := xs[i] - mx
		num += dx * (ys[i] - my)
		den += dx * dx
	}
	if den == 0 { // all prices equal after filtering — no variation
		return Elasticity{
			Method:      "insufficient-variation",
			Points:      len(xs),
			Explanation: "observed prices carry no variation; vary the price to measure elasticity",
		}
	}
	e := num / den
	return Elasticity{
		Coefficient: round2(e),
		Points:      len(xs),
		Method:      "log-log-ols",
		Explanation: elasticityExplanation(e),
	}
}

func elasticityExplanation(e float64) string {
	switch {
	case e <= -1-unitBand:
		return fmt.Sprintf("elastic (e=%.2f): demand is price-sensitive, so a lower price should grow revenue", e)
	case e >= -1+unitBand:
		return fmt.Sprintf("inelastic (e=%.2f): demand holds as price rises, so a higher price should grow revenue", e)
	default:
		return fmt.Sprintf("near unit-elastic (e=%.2f): revenue is roughly flat across price, so hold", e)
	}
}

// recommend turns an elasticity estimate into a bounded price move. Revenue scales
// as price^(1+e): when 1+e>0 revenue rises with price (inelastic → raise); when
// 1+e<0 it falls (elastic → cut); near unit-elastic it is flat (hold).
func recommend(current int64, currency string, e Elasticity) Recommendation {
	if !e.Estimated() {
		return holdRecommendation(current, currency,
			"not enough price variation to estimate elasticity — hold and A/B test two prices")
	}
	if current <= 0 {
		return holdRecommendation(current, currency,
			"current price unknown — set a base price before optimizing")
	}
	exponent := 1 + e.Coefficient
	if math.Abs(exponent) < unitBand {
		return Recommendation{
			CurrentPrice: current, RecommendedPrice: current, Currency: currency,
			Rationale: fmt.Sprintf("near unit-elastic (e=%.2f): revenue barely moves with price — hold", e.Coefficient),
		}
	}

	dir := 1.0 // raise
	if exponent < 0 {
		dir = -1.0 // cut
	}
	newPrice := int64(math.Round(float64(current) * (1 + dir*maxStep)))
	if newPrice < 1 {
		newPrice = 1
	}
	lift := math.Pow(float64(newPrice)/float64(current), exponent) - 1

	verb := "raise"
	if dir < 0 {
		verb = "cut"
	}
	return Recommendation{
		CurrentPrice:     current,
		RecommendedPrice: newPrice,
		Currency:         currency,
		ExpectedLift:     round4(lift),
		Rationale: fmt.Sprintf("%s price %d%%→%d (e=%.2f): revenue scales as price^%.2f, projecting %+.1f%% revenue",
			verb, pct(current, newPrice), newPrice, e.Coefficient, exponent, lift*100),
	}
}

func holdRecommendation(current int64, currency, why string) Recommendation {
	return Recommendation{
		CurrentPrice: current, RecommendedPrice: current, Currency: currency, Rationale: why,
	}
}

// modalPrice returns the price with the most calls (ties broken by lower price),
// used as the recommendation anchor when no advertised price is available.
func modalPrice(points []PricePoint) int64 {
	var best PricePoint
	for i, p := range points {
		if i == 0 || p.Calls > best.Calls || (p.Calls == best.Calls && p.Price < best.Price) {
			best = p
		}
	}
	return best.Price
}

func pct(from, to int64) int {
	if from == 0 {
		return 0
	}
	return int(math.Round(float64(to-from) / float64(from) * 100))
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round4(f float64) float64 { return math.Round(f*10000) / 10000 }
