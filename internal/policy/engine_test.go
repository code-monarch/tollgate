package policy

import (
	"context"
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

func baseReq() Request {
	return Request{
		AgentID: "agt_1", TaskID: "task_1", ServiceID: "svc_1",
		Amount: 1000, Currency: "USDC", ResourceHost: "api.example.com",
		ServiceCategory: "geo", Balance: 1_000_000, Now: t0,
	}
}

func eval(t *testing.T, rules []Rule, req Request) Decision {
	t.Helper()
	return evalWith(t, NewMemTracker(), rules, req)
}

func evalWith(t *testing.T, tr Tracker, rules []Rule, req Request) Decision {
	t.Helper()
	e := NewEngine(tr)
	d, err := e.Evaluate(context.Background(), Policy{Currency: "USDC", Defaults: Defaults{Action: Deny}, Rules: rules}, req)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return d
}

func TestNoRules_UsesDefault(t *testing.T) {
	e := NewEngine(NewMemTracker())
	d, _ := e.Evaluate(context.Background(), Policy{Defaults: Defaults{Action: Deny}}, baseReq())
	if d.Decision != Deny {
		t.Fatalf("no rules should fall to default deny, got %s", d.Decision)
	}
}

func TestAmountCeiling(t *testing.T) {
	rules := []Rule{{ID: "ceil", Type: TypeAmountCeiling, Max: "5000"}}
	if d := eval(t, rules, baseReq()); d.Decision != Allow {
		t.Fatalf("1000 under 5000 ceiling should allow, got %s", d.Decision)
	}
	req := baseReq()
	req.Amount = 6000
	d := eval(t, rules, req)
	if d.Decision != Deny || len(d.FiredRules) != 1 || d.FiredRules[0] != "ceil" {
		t.Fatalf("6000 over ceiling: %+v", d)
	}
}

func TestBudget_Task(t *testing.T) {
	tr := NewMemTracker()
	tr.Record(context.Background(), Event{AgentID: "agt_1", TaskID: "task_1", Currency: "USDC", Amount: 4500, At: t0})
	rules := []Rule{{ID: "task-budget", Type: TypeBudget, Scope: "task", Window: "task", Max: "5000"}}

	// 4500 spent + 1000 = 5500 > 5000 → deny.
	if d := evalWith(t, tr, rules, baseReq()); d.Decision != Deny {
		t.Fatalf("task budget should deny, got %+v", d)
	}
	// A different task is unaffected.
	req := baseReq()
	req.TaskID = "task_2"
	if d := evalWith(t, tr, rules, req); d.Decision != Allow {
		t.Fatalf("other task should allow, got %+v", d)
	}
}

func TestBudget_RollingWindow(t *testing.T) {
	tr := NewMemTracker()
	// Two charges: one inside the 24h window, one outside.
	tr.Record(context.Background(), Event{AgentID: "agt_1", Currency: "USDC", Amount: 19_000_000, At: t0.Add(-1 * time.Hour)})
	tr.Record(context.Background(), Event{AgentID: "agt_1", Currency: "USDC", Amount: 19_000_000, At: t0.Add(-48 * time.Hour)})
	rules := []Rule{{ID: "daily", Type: TypeBudget, Scope: "agent", Window: "24h", Max: "20000000"}}

	req := baseReq()
	req.Amount = 2_000_000
	// In-window spend 19M + 2M = 21M > 20M → deny (the 48h-old charge is excluded).
	if d := evalWith(t, tr, rules, req); d.Decision != Deny {
		t.Fatalf("daily budget should deny counting only in-window spend, got %+v", d)
	}
}

func TestAllowlist_HostGlob(t *testing.T) {
	rules := []Rule{{ID: "hosts", Type: TypeAllowlist, Field: "resource_host", Values: []string{"api.example.com", "*.trusted.dev"}}}

	if d := eval(t, rules, baseReq()); d.Decision != Allow {
		t.Fatalf("exact host should allow, got %+v", d)
	}
	req := baseReq()
	req.ResourceHost = "svc.trusted.dev"
	if d := eval(t, rules, req); d.Decision != Allow {
		t.Fatalf("glob host should allow, got %+v", d)
	}
	req.ResourceHost = "evil.com"
	if d := eval(t, rules, req); d.Decision != Deny {
		t.Fatalf("non-allowlisted host should deny, got %+v", d)
	}
}

func TestBlocklist_Category(t *testing.T) {
	rules := []Rule{{ID: "cats", Type: TypeBlocklist, Field: "service_category", Values: []string{"adult", "gambling"}}}
	if d := eval(t, rules, baseReq()); d.Decision != Allow {
		t.Fatalf("geo not blocked should allow, got %+v", d)
	}
	req := baseReq()
	req.ServiceCategory = "gambling"
	if d := eval(t, rules, req); d.Decision != Deny {
		t.Fatalf("blocked category should deny, got %+v", d)
	}
}

func TestVelocity(t *testing.T) {
	tr := NewMemTracker()
	for i := 0; i < 100; i++ {
		tr.Record(context.Background(), Event{AgentID: "agt_1", Currency: "USDC", Amount: 1, At: t0.Add(-30 * time.Second)})
	}
	rules := []Rule{{ID: "rate", Type: TypeVelocity, Scope: "agent", MaxCount: 100, Window: "1m"}}
	if d := evalWith(t, tr, rules, baseReq()); d.Decision != Deny {
		t.Fatalf("100 in 1m at cap 100 should deny, got %+v", d)
	}
	// Older-than-window events don't count.
	tr2 := NewMemTracker()
	tr2.Record(context.Background(), Event{AgentID: "agt_1", Currency: "USDC", Amount: 1, At: t0.Add(-2 * time.Minute)})
	if d := evalWith(t, tr2, rules, baseReq()); d.Decision != Allow {
		t.Fatalf("stale events should not trip velocity, got %+v", d)
	}
}

func TestAnomaly_PriceSpike(t *testing.T) {
	rules := []Rule{{ID: "spike", Type: TypeAnomaly, Signal: "price_spike", Factor: 5}}
	req := baseReq()
	req.MedianPrice = 1000
	req.Amount = 6000 // > 5x median
	if d := eval(t, rules, req); d.Decision != NeedsApproval {
		t.Fatalf("price spike should need approval, got %+v", d)
	}
	req.Amount = 4000 // < 5x
	if d := eval(t, rules, req); d.Decision != Allow {
		t.Fatalf("within factor should allow, got %+v", d)
	}
}

func TestApproval_Threshold(t *testing.T) {
	rules := []Rule{{ID: "approve", Type: TypeApproval, Threshold: "1000000"}}
	req := baseReq()
	req.Amount = 1_000_000
	if d := eval(t, rules, req); d.Decision != NeedsApproval || d.FiredRules[0] != "approve" {
		t.Fatalf("at threshold should need approval, got %+v", d)
	}
	req.Amount = 999_999
	if d := eval(t, rules, req); d.Decision != Allow {
		t.Fatalf("below threshold should allow, got %+v", d)
	}
}

func TestInsufficientFunds(t *testing.T) {
	req := baseReq()
	req.Balance = 500
	req.Amount = 1000
	d := eval(t, []Rule{{ID: "ceil", Type: TypeAmountCeiling, Max: "5000"}}, req)
	if d.Decision != Deny || d.Reason != "insufficient funds" {
		t.Fatalf("insufficient funds should deny distinctly, got %+v", d)
	}
}

func TestMostRestrictiveWins(t *testing.T) {
	// Approval (needs_approval) + ceiling breach (deny) → deny wins.
	rules := []Rule{
		{ID: "approve", Type: TypeApproval, Threshold: "500"},
		{ID: "ceil", Type: TypeAmountCeiling, Max: "800"},
	}
	req := baseReq()
	req.Amount = 1000 // over ceiling AND over approval threshold
	d := eval(t, rules, req)
	if d.Decision != Deny {
		t.Fatalf("deny must beat needs_approval, got %s", d.Decision)
	}
	// Both rules fired.
	if len(d.FiredRules) != 2 {
		t.Fatalf("expected both rules fired, got %v", d.FiredRules)
	}
}

func TestFullPolicy_Allows(t *testing.T) {
	// A realistic stacked policy that should allow a normal call.
	rules := []Rule{
		{ID: "ceil", Type: TypeAmountCeiling, Scope: "call", Max: "50000"},
		{ID: "daily", Type: TypeBudget, Scope: "agent", Window: "24h", Max: "20000000"},
		{ID: "hosts", Type: TypeAllowlist, Field: "resource_host", Values: []string{"api.example.com"}},
		{ID: "cats", Type: TypeBlocklist, Field: "service_category", Values: []string{"adult"}},
		{ID: "rate", Type: TypeVelocity, Scope: "agent", MaxCount: 100, Window: "1m"},
		{ID: "approve", Type: TypeApproval, Threshold: "1000000"},
	}
	if d := eval(t, rules, baseReq()); d.Decision != Allow || len(d.FiredRules) != 0 {
		t.Fatalf("normal call should cleanly allow, got %+v", d)
	}
}
