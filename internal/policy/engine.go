package policy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Engine evaluates policies. It is deterministic: the only external reads are
// the balance (passed in the Request) and spend/velocity history (Tracker).
type Engine struct {
	tracker Tracker
}

// NewEngine builds an engine over a spend/velocity tracker.
func NewEngine(t Tracker) *Engine { return &Engine{tracker: t} }

// rank orders verdicts by restrictiveness so the most restrictive wins.
func rank(a Action) int {
	switch a {
	case Deny:
		return 2
	case NeedsApproval:
		return 1
	default:
		return 0
	}
}

// Evaluate applies a policy to a request and returns the decision plus the rules
// that fired. Algorithm (docs/05-policy-engine.md):
//
//  1. Baseline is defaults.action when there are no rules; otherwise allow.
//  2. Evaluate every rule; combine most-restrictive (deny > needs_approval > allow).
//  3. Insufficient balance is a distinct deny.
//  4. The decision + fired rules are returned for the caller to audit-log.
func (e *Engine) Evaluate(ctx context.Context, p Policy, req Request) (Decision, error) {
	if req.Now.IsZero() {
		req.Now = time.Now()
	}

	decision := Allow
	if len(p.Rules) == 0 {
		decision = p.Defaults.Action
	}
	var fired []string
	reason := ""

	promote := func(v Action, ruleID, why string) {
		if rank(v) > rank(decision) {
			decision = v
			reason = why
		}
		if v != Allow {
			fired = append(fired, ruleID)
		}
	}

	for _, r := range p.Rules {
		v, why, err := e.evalRule(ctx, r, req)
		if err != nil {
			return Decision{}, err
		}
		promote(v, r.ID, why)
	}

	// The learning boundary is deny-by-default, so it is enforced here rather than
	// inside the rule loop: a policy that carries no exhaust_rights rule grants
	// nothing, and a seller that demands rights anyway must be refused. Silence
	// never grants (docs/08-learning-boundary.md).
	if ungrantable := notIn(req.RequiredRights, GrantableRights(p)); len(ungrantable) > 0 {
		promote(Deny, exhaustRuleID(p), fmt.Sprintf(
			"service requires exhaust rights this policy will not grant: %s",
			strings.Join(ungrantable, ", ")))
	}

	// Insufficient funds is a hard deny with a distinct reason.
	if req.Balance < req.Amount {
		promote(Deny, "insufficient-funds", "insufficient funds")
	}

	return Decision{Decision: decision, FiredRules: fired, Reason: reason}, nil
}

// notIn returns the members of want that are absent from have.
func notIn(want, have []string) []string {
	if len(want) == 0 {
		return nil
	}
	haveSet := make(map[string]bool, len(have))
	for _, h := range have {
		haveSet[h] = true
	}
	var missing []string
	for _, w := range want {
		if !haveSet[w] {
			missing = append(missing, w)
		}
	}
	return missing
}

// exhaustRuleID names the rule to blame in the audit log: the policy's own
// exhaust_rights rule if it has one, otherwise the implicit closed boundary.
func exhaustRuleID(p Policy) string {
	for _, r := range p.Rules {
		if r.Type == TypeExhaustRights {
			return r.ID
		}
	}
	return "exhaust-rights-default-deny"
}

// evalRule returns a single rule's verdict, a human reason, and any error.
func (e *Engine) evalRule(ctx context.Context, r Rule, req Request) (Action, string, error) {
	switch r.Type {
	case TypeAmountCeiling:
		max, err := parseAmount(r.Max)
		if err != nil {
			return Deny, "", err
		}
		if req.Amount > max {
			return Deny, fmt.Sprintf("amount %d exceeds ceiling %d", req.Amount, max), nil
		}
		return Allow, "", nil

	case TypeBudget:
		max, err := parseAmount(r.Max)
		if err != nil {
			return Deny, "", err
		}
		spent, err := e.budgetSpend(ctx, r, req)
		if err != nil {
			return Deny, "", err
		}
		if spent+req.Amount > max {
			return Deny, fmt.Sprintf("budget exceeded: %d + %d > %d", spent, req.Amount, max), nil
		}
		return Allow, "", nil

	case TypeAllowlist:
		if matchField(r, req) {
			return Allow, "", nil
		}
		return Deny, fmt.Sprintf("%s not in allowlist", r.Field), nil

	case TypeBlocklist:
		if matchField(r, req) {
			return Deny, fmt.Sprintf("%s is blocked", r.Field), nil
		}
		return Allow, "", nil

	case TypeVelocity:
		window, err := parseWindow(r.Window)
		if err != nil {
			return Deny, "", err
		}
		n, err := e.tracker.AgentCountSince(ctx, req.AgentID, req.Now.Add(-window))
		if err != nil {
			return Deny, "", err
		}
		if n >= r.MaxCount {
			return Deny, fmt.Sprintf("velocity cap: %d in %s >= %d", n, r.Window, r.MaxCount), nil
		}
		return Allow, "", nil

	case TypeAnomaly:
		if req.MedianPrice > 0 && r.Factor > 0 {
			if float64(req.Amount) > r.Factor*float64(req.MedianPrice) {
				return NeedsApproval, fmt.Sprintf("price spike: %d > %.1fx median %d", req.Amount, r.Factor, req.MedianPrice), nil
			}
		}
		return Allow, "", nil

	case TypeApproval:
		threshold, err := parseAmount(r.Threshold)
		if err != nil {
			return Deny, "", err
		}
		if req.Amount >= threshold {
			return NeedsApproval, fmt.Sprintf("amount %d >= approval threshold %d", req.Amount, threshold), nil
		}
		return Allow, "", nil

	case TypeExhaustRights:
		// Declarative, not restrictive: this rule widens what the agent MAY grant.
		// The refusal it implies is enforced centrally in Evaluate, because it must
		// hold even for a policy that carries no exhaust_rights rule at all.
		return Allow, "", nil

	default:
		return Deny, "", fmt.Errorf("policy: unknown rule type %q", r.Type)
	}
}

// budgetSpend returns prior spend relevant to a budget rule's scope+window.
func (e *Engine) budgetSpend(ctx context.Context, r Rule, req Request) (int64, error) {
	if r.Window == "task" || r.Scope == "task" {
		return e.tracker.TaskSpend(ctx, req.AgentID, req.TaskID, req.Currency)
	}
	window, err := parseWindow(r.Window)
	if err != nil {
		return 0, err
	}
	return e.tracker.AgentSpendSince(ctx, req.AgentID, req.Currency, req.Now.Add(-window))
}

// matchField reports whether the request's value for r.Field matches any of
// r.Values (host globs like "*.trusted.dev" are supported).
func matchField(r Rule, req Request) bool {
	var val string
	switch r.Field {
	case "resource_host":
		val = req.ResourceHost
	case "service_category":
		val = req.ServiceCategory
	default:
		return false
	}
	for _, pattern := range r.Values {
		if hostMatch(pattern, val) {
			return true
		}
	}
	return false
}

// hostMatch supports exact matches and a single leading "*." wildcard.
func hostMatch(pattern, val string) bool {
	if pattern == val {
		return true
	}
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return val == suffix || strings.HasSuffix(val, "."+suffix)
	}
	return false
}

func parseAmount(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("policy: empty amount")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("policy: invalid amount %q", s)
	}
	return n, nil
}

// parseWindow accepts a Go duration ("24h", "1h", "1m"). "task" is handled by
// the caller and is not a time window.
func parseWindow(s string) (time.Duration, error) {
	if s == "" || s == "task" {
		return 0, fmt.Errorf("policy: window %q is not a duration", s)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("policy: invalid window %q: %w", s, err)
	}
	return d, nil
}
