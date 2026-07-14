package analytics

import (
	"math"
	"testing"
	"time"

	"github.com/tollgate/tollgate/internal/ledger"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// txns builds n settled transactions for a service at a price by one agent.
func txns(service, agent string, price int64, n int) []ledger.Transaction {
	out := make([]ledger.Transaction, n)
	for i := range out {
		out[i] = ledger.Transaction{
			ID: agent + "-" + service, AgentID: agent, ServiceID: service,
			Amount: price, Currency: "USDC", Status: ledger.StatusSettled,
			CreatedAt: epoch.Add(time.Duration(i) * time.Minute),
		}
	}
	return out
}

func TestComputeRevenueAndCohorts(t *testing.T) {
	var all []ledger.Transaction
	all = append(all, txns("svc", "agt_alpha", 1000, 12)...)
	all = append(all, txns("svc", "agt_beta", 1000, 6)...)
	all = append(all, txns("svc", "agt_alpha", 1500, 4)...)

	rep := Compute("svc", all, 1000, "USDC")

	if rep.Calls != 22 {
		t.Fatalf("calls = %d, want 22", rep.Calls)
	}
	if want := int64(12*1000 + 6*1000 + 4*1500); rep.Revenue != want {
		t.Fatalf("revenue = %d, want %d", rep.Revenue, want)
	}
	if rep.UniqueCallers != 2 {
		t.Fatalf("unique callers = %d, want 2", rep.UniqueCallers)
	}
	if rep.AvgPrice != rep.Revenue/rep.Calls {
		t.Fatalf("avg price = %d, want %d", rep.AvgPrice, rep.Revenue/rep.Calls)
	}
	// Cohorts sorted by spend desc: alpha (12k+6k=18k) before beta (6k).
	if rep.Cohorts[0].AgentID != "agt_alpha" || rep.Cohorts[0].Spend != 18000 {
		t.Fatalf("top cohort = %+v, want agt_alpha spend 18000", rep.Cohorts[0])
	}
	if rep.Cohorts[1].AgentID != "agt_beta" || rep.Cohorts[1].Spend != 6000 {
		t.Fatalf("second cohort = %+v, want agt_beta spend 6000", rep.Cohorts[1])
	}
	// Price points sorted ascending by price, with correct demand.
	if len(rep.PricePoints) != 2 || rep.PricePoints[0].Price != 1000 || rep.PricePoints[1].Price != 1500 {
		t.Fatalf("price points = %+v", rep.PricePoints)
	}
	if rep.PricePoints[0].Calls != 18 || rep.PricePoints[1].Calls != 4 {
		t.Fatalf("price point demand = %+v", rep.PricePoints)
	}
}

func TestElasticityElasticRecommendsCut(t *testing.T) {
	var all []ledger.Transaction
	all = append(all, txns("svc", "agt_alpha", 1000, 18)...)
	all = append(all, txns("svc", "agt_alpha", 1500, 4)...) // higher price, far less demand

	rep := Compute("svc", all, 1000, "USDC")

	if !rep.Elasticity.Estimated() {
		t.Fatalf("expected an elasticity estimate, got %+v", rep.Elasticity)
	}
	if rep.Elasticity.Coefficient >= -1 {
		t.Fatalf("coefficient = %v, want elastic (< -1)", rep.Elasticity.Coefficient)
	}
	// Elastic demand => recommend a price cut, bounded to -15%.
	if rep.Recommendation.RecommendedPrice >= 1000 {
		t.Fatalf("recommended price = %d, want a cut below 1000", rep.Recommendation.RecommendedPrice)
	}
	if rep.Recommendation.RecommendedPrice != 850 {
		t.Fatalf("recommended price = %d, want 850 (-15%%)", rep.Recommendation.RecommendedPrice)
	}
}

func TestElasticityInelasticRecommendsRaise(t *testing.T) {
	var all []ledger.Transaction
	// Price doubles, demand barely falls => inelastic.
	all = append(all, txns("svc", "agt_alpha", 1000, 20)...)
	all = append(all, txns("svc", "agt_alpha", 2000, 18)...)

	rep := Compute("svc", all, 1000, "USDC")

	if rep.Elasticity.Coefficient <= -1 {
		t.Fatalf("coefficient = %v, want inelastic (> -1)", rep.Elasticity.Coefficient)
	}
	if rep.Recommendation.RecommendedPrice != 1150 {
		t.Fatalf("recommended price = %d, want 1150 (+15%%)", rep.Recommendation.RecommendedPrice)
	}
	if rep.Recommendation.ExpectedLift <= 0 {
		t.Fatalf("expected lift = %v, want positive", rep.Recommendation.ExpectedLift)
	}
}

func TestInsufficientVariationHolds(t *testing.T) {
	rep := Compute("svc", txns("svc", "agt_alpha", 1000, 10), 1000, "USDC")

	if rep.Elasticity.Estimated() {
		t.Fatalf("single price should not yield an estimate: %+v", rep.Elasticity)
	}
	if rep.Recommendation.RecommendedPrice != 1000 {
		t.Fatalf("recommended price = %d, want hold at 1000", rep.Recommendation.RecommendedPrice)
	}
}

func TestEmptyWindow(t *testing.T) {
	rep := Compute("svc", nil, 1000, "USDC")
	if rep.Calls != 0 || rep.Revenue != 0 {
		t.Fatalf("empty report has calls=%d revenue=%d", rep.Calls, rep.Revenue)
	}
	if rep.Recommendation.RecommendedPrice != 1000 {
		t.Fatalf("empty window should hold price, got %d", rep.Recommendation.RecommendedPrice)
	}
}

func TestUnknownPriceAnchorsOnModal(t *testing.T) {
	var all []ledger.Transaction
	all = append(all, txns("svc", "agt_alpha", 1000, 18)...) // modal price
	all = append(all, txns("svc", "agt_alpha", 1500, 4)...)

	rep := Compute("svc", all, 0, "USDC") // current price unknown
	if rep.Recommendation.CurrentPrice != 1000 {
		t.Fatalf("anchor = %d, want modal 1000", rep.Recommendation.CurrentPrice)
	}
}

func TestElasticityMathMatchesOLS(t *testing.T) {
	var all []ledger.Transaction
	all = append(all, txns("svc", "a", 1000, 12)...)
	all = append(all, txns("svc", "a", 1500, 4)...)
	rep := Compute("svc", all, 1000, "USDC")

	// Two points: slope = (ln4-ln12)/(ln1500-ln1000).
	want := (math.Log(4) - math.Log(12)) / (math.Log(1500) - math.Log(1000))
	if math.Abs(rep.Elasticity.Coefficient-round2(want)) > 0.01 {
		t.Fatalf("coefficient = %v, want ~%v", rep.Elasticity.Coefficient, round2(want))
	}
}

// A seller that learns from its customers cannot book that knowledge as free: the
// dividend it paid shows up against its revenue (docs/08-learning-boundary.md).
func TestCompute_DividendSplitsGrossFromNetRevenue(t *testing.T) {
	all := txns("svc", "agt_alpha", 1000, 3)
	// Two of the three callers granted rights and were paid 200 each.
	all[0].Rebate = 200
	all[1].Rebate = 200

	rep := Compute("svc", all, 1000, "USDC")

	if rep.Revenue != 3000 {
		t.Fatalf("gross revenue = %d, want 3000", rep.Revenue)
	}
	if rep.DividendPaid != 400 {
		t.Fatalf("dividend paid = %d, want 400", rep.DividendPaid)
	}
	if rep.NetRevenue != 2600 {
		t.Fatalf("net revenue = %d, want 2600 (3000 earned - 400 paid to learn)", rep.NetRevenue)
	}
	if rep.Cohorts[0].DividendEarned != 400 {
		t.Fatalf("caller's dividend = %d, want 400 — what its knowledge was worth", rep.Cohorts[0].DividendEarned)
	}
}
