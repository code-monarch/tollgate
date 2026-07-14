package analytics_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tollgate/tollgate/internal/analytics"
	"github.com/tollgate/tollgate/internal/facilitator"
	"github.com/tollgate/tollgate/internal/ledger"
	"github.com/tollgate/tollgate/internal/pricing"
	"github.com/tollgate/tollgate/internal/registry"
	"github.com/tollgate/tollgate/internal/settlement"
	"github.com/tollgate/tollgate/x402"
)

// buyer is a registered, funded agent that can settle real payments.
type buyer struct {
	id, wallet string
	key        ed25519.PrivateKey
}

// e2e wires a real facilitator over a shared ledger plus a catalog, so analytics
// reads exactly the transactions the payment flow settled — no synthetic data.
type e2e struct {
	core  *facilitator.Core
	store ledger.Store
	reg   *registry.MemStore
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	signer, err := x402.NewSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	store := ledger.NewMemStore()
	core := facilitator.NewCore(store, signer, settlement.Mock{})
	return &e2e{core: core, store: store, reg: registry.NewMemStore()}
}

// service registers a sellable endpoint in both the facilitator and the catalog.
func (e *e2e) service(t *testing.T, id, sellerWallet string, pricing registry.Pricing) {
	t.Helper()
	e.core.RegisterService(facilitator.Service{
		ID: id, SellerWallet: sellerWallet, Currency: "USDC",
		Network: "base", Asset: "USDC", PayTo: "0xSELLER",
	})
	if err := e.reg.Put(context.Background(), registry.Service{
		ID: id, Name: id, SellerWallet: sellerWallet, Pricing: pricing,
		SLA: registry.SLA{Uptime: 0.999},
	}); err != nil {
		t.Fatal(err)
	}
}

// agent registers and funds a buyer.
func (e *e2e) agent(t *testing.T, id, wallet string) buyer {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	e.core.RegisterAgent(facilitator.Agent{ID: id, Wallet: wallet, PublicKey: pub})
	if err := e.core.Fund(context.Background(), id, "100000000", "USDC"); err != nil {
		t.Fatal(err)
	}
	return buyer{id: id, wallet: wallet, key: priv}
}

// buy settles n real payments from b for service at price. Each goes through
// IssueQuote → SignPayment → Settle, exactly like a live buyer.
func (e *e2e) buy(t *testing.T, b buyer, service, price string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		q, err := e.core.IssueQuote(ctx, facilitator.QuoteRequest{
			ServiceID: service, Amount: price, Resource: fmt.Sprintf("/call?i=%d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		p := x402.Payment{QuoteID: q.QuoteID, Nonce: q.Nonce, AgentID: b.id, PayFrom: b.wallet}
		x402.SignPayment(b.key, &p)
		hash := fmt.Sprintf("%s:%s:%s:%d", b.id, service, price, i)
		res, err := e.core.Settle(ctx, q.QuoteID, p, hash, false)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Settled || !res.Fresh {
			t.Fatalf("settle %s@%s #%d: settled=%v fresh=%v reason=%q", service, price, i, res.Settled, res.Fresh, res.Reason)
		}
	}
}

// TestAnalyticsEndToEnd drives real payments through the facilitator, then reads
// the analytics report off the same ledger and asserts revenue, cohorts and an
// elastic-demand price-cut recommendation.
func TestAnalyticsEndToEnd(t *testing.T) {
	e := newE2E(t)
	e.service(t, "svc_geo", "wallet:seller_geo", registry.Pricing{Model: "static", Amount: "1000", Currency: "USDC"})
	alpha := e.agent(t, "agt_alpha", "wallet:alpha")
	beta := e.agent(t, "agt_beta", "wallet:beta")

	// Elastic demand: the cheap price sells far more than the expensive one.
	e.buy(t, alpha, "svc_geo", "1000", 12)
	e.buy(t, beta, "svc_geo", "1000", 6)
	e.buy(t, alpha, "svc_geo", "1500", 4)

	rep, err := analytics.Service(context.Background(), e.store, e.reg, "svc_geo", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Calls != 22 {
		t.Fatalf("calls = %d, want 22", rep.Calls)
	}
	if want := int64(12*1000 + 6*1000 + 4*1500); rep.Revenue != want {
		t.Fatalf("revenue = %d, want %d", rep.Revenue, want)
	}
	if rep.UniqueCallers != 2 {
		t.Fatalf("unique callers = %d, want 2", rep.UniqueCallers)
	}
	if rep.Cohorts[0].AgentID != "agt_alpha" {
		t.Fatalf("top cohort = %s, want agt_alpha", rep.Cohorts[0].AgentID)
	}
	if !rep.Elasticity.Estimated() || rep.Elasticity.Coefficient >= -1 {
		t.Fatalf("elasticity = %+v, want an elastic (< -1) estimate", rep.Elasticity)
	}
	if rep.Recommendation.RecommendedPrice >= 1000 {
		t.Fatalf("recommended price = %d, want a cut below the 1000 base", rep.Recommendation.RecommendedPrice)
	}

	// Confirm the ledger actually moved: seller earned exactly the reported revenue.
	if bal, _ := e.core.Balance(context.Background(), "wallet:seller_geo", "USDC"); bal != rep.Revenue {
		t.Fatalf("seller balance %d != reported revenue %d", bal, rep.Revenue)
	}
}

// TestAnalyticsHTTP exercises the wire path: the report and the dynamic-price
// resolution served over HTTP off real settled traffic.
func TestAnalyticsHTTP(t *testing.T) {
	e := newE2E(t)
	e.service(t, "svc_geo", "wallet:seller_geo", registry.Pricing{Model: "static", Amount: "1000", Currency: "USDC"})
	e.service(t, "svc_dyn", "wallet:seller_dyn", registry.Pricing{
		Model: "dynamic", Amount: "500", Currency: "USDC", Floor: 250, Ceiling: 750, TargetRate: 10, MaxSurge: 0.5,
	})
	alpha := e.agent(t, "agt_alpha", "wallet:alpha")

	e.buy(t, alpha, "svc_geo", "1000", 5)
	e.buy(t, alpha, "svc_dyn", "500", 14) // 14 calls vs target 10 → surge

	srv := analytics.NewServer(e.store, e.reg)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	// Report over HTTP.
	var rep analytics.Report
	getJSON(t, ts.URL+"/v1/analytics/services/svc_geo", &rep)
	if rep.Revenue != 5000 || rep.Calls != 5 {
		t.Fatalf("report over HTTP: revenue=%d calls=%d, want 5000/5", rep.Revenue, rep.Calls)
	}

	// Dynamic price resolution over HTTP: util 1.4 → +20% → 600.
	var resolved pricing.Resolved
	getJSON(t, ts.URL+"/v1/pricing/services/svc_dyn", &resolved)
	if resolved.Model != "dynamic" {
		t.Fatalf("model = %s, want dynamic", resolved.Model)
	}
	if resolved.Price != 600 {
		t.Fatalf("dynamic price = %d, want 600 (14 calls vs target 10 → +20%%)", resolved.Price)
	}

	// A static service resolves to its base regardless of load.
	getJSON(t, ts.URL+"/v1/pricing/services/svc_geo", &resolved)
	if resolved.Price != 1000 || resolved.Model != "static" {
		t.Fatalf("static resolve = %d/%s, want 1000/static", resolved.Price, resolved.Model)
	}
}

func getJSON(t *testing.T, url string, dst any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s → %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatal(err)
	}
}
