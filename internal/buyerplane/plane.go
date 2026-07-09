// Package buyerplane is the custodial buyer side: agent identities and wallets,
// versioned policies, and the authorize/pay flow that runs every intended
// payment through the policy engine before funds move (docs/02-architecture.md
// buyer plane, docs/05-policy-engine.md, docs/06-api-spec.md).
//
// It custodies agent signing keys (MPC/custodial to start, per the overview) so
// it can construct payments on an agent's behalf once a payment is authorized.
package buyerplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/tollgate/tollgate/internal/buyer"
	"github.com/tollgate/tollgate/internal/facilitator"
	"github.com/tollgate/tollgate/internal/policy"
	"github.com/tollgate/tollgate/x402"
)

// Agent is a custodial buyer identity.
type Agent struct {
	ID     string
	Label  string
	Wallet string
	priv   ed25519.PrivateKey
	pub    ed25519.PublicKey
	scope  policy.Scope
}

// Plane is the buyer-side service.
type Plane struct {
	mu        sync.Mutex
	fac       *facilitator.Core
	facPub    ed25519.PublicKey
	policies  *policy.Store
	engine    *policy.Engine
	tracker   policy.Tracker
	approvals *policy.ApprovalManager
	agents    map[string]*Agent
	now       func() time.Time
}

// NewPlane builds a buyer plane over a facilitator core. facPub is the
// facilitator's quote-signing key, used to verify quotes before paying.
func NewPlane(fac *facilitator.Core, facPub ed25519.PublicKey) *Plane {
	tracker := policy.NewMemTracker()
	return &Plane{
		fac:       fac,
		facPub:    facPub,
		policies:  policy.NewStore(),
		engine:    policy.NewEngine(tracker),
		tracker:   tracker,
		approvals: policy.NewApprovalManager(),
		agents:    make(map[string]*Agent),
		now:       time.Now,
	}
}

// CreateAgent mints a custodial agent: a keypair, a wallet, and registration
// with the facilitator so its payments verify.
func (p *Plane) CreateAgent(_ context.Context, label string) (Agent, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Agent{}, err
	}
	id := newID("agt")
	a := &Agent{
		ID: id, Label: label, Wallet: "wallet:" + id,
		priv: priv, pub: pub, scope: policy.Scope{Type: "agent", ID: id},
	}
	p.mu.Lock()
	p.agents[id] = a
	p.mu.Unlock()

	p.fac.RegisterAgent(facilitator.Agent{ID: a.ID, Wallet: a.Wallet, PublicKey: pub})
	return *a, nil
}

// Fund tops up an agent's wallet.
func (p *Plane) Fund(ctx context.Context, agentID, amount, currency string) error {
	if _, ok := p.agent(agentID); !ok {
		return fmt.Errorf("buyerplane: unknown agent %q", agentID)
	}
	return p.fac.Fund(ctx, agentID, amount, currency)
}

// Balance returns an agent's derived balance.
func (p *Plane) Balance(ctx context.Context, agentID, currency string) (int64, error) {
	a, ok := p.agent(agentID)
	if !ok {
		return 0, fmt.Errorf("buyerplane: unknown agent %q", agentID)
	}
	return p.fac.Balance(ctx, a.Wallet, currency)
}

// CreatePolicy stores a new active policy version for an agent.
func (p *Plane) CreatePolicy(ctx context.Context, agentID string, pol policy.Policy) (policy.Policy, error) {
	a, ok := p.agent(agentID)
	if !ok {
		return policy.Policy{}, fmt.Errorf("buyerplane: unknown agent %q", agentID)
	}
	return p.policies.Create(ctx, a.scope, pol)
}

// ActivePolicy returns an agent's active policy.
func (p *Plane) ActivePolicy(ctx context.Context, agentID string) (policy.Policy, bool) {
	a, ok := p.agent(agentID)
	if !ok {
		return policy.Policy{}, false
	}
	return p.policies.Active(ctx, a.scope)
}

// Decision is the buyer plane's authorize/pay outcome.
type Decision struct {
	Decision          policy.Action `json:"decision"`
	FiredRules        []string      `json:"firedRules"`
	Reason            string        `json:"reason,omitempty"`
	ApprovalRequestID string        `json:"approvalRequestId,omitempty"`
}

// AuthorizeInput carries the context needed to decide on a quote.
type AuthorizeInput struct {
	AgentID         string
	TaskID          string
	Quote           x402.Quote
	ServiceCategory string
	MedianPrice     int64
}

// Authorize runs the policy engine against a quote without paying. On a
// needs_approval outcome it creates (or reuses) an approval request and returns
// its id. No policy for the agent means deny (safe posture).
func (p *Plane) Authorize(ctx context.Context, in AuthorizeInput) (Decision, error) {
	a, ok := p.agent(in.AgentID)
	if !ok {
		return Decision{}, fmt.Errorf("buyerplane: unknown agent %q", in.AgentID)
	}
	if err := x402.VerifyQuote(p.facPub, in.Quote); err != nil {
		return Decision{}, fmt.Errorf("buyerplane: quote failed verification: %w", err)
	}

	req, err := p.policyRequest(ctx, a, in)
	if err != nil {
		return Decision{}, err
	}
	pol, ok := p.policies.Active(ctx, a.scope)
	if !ok {
		// No policy → deny by default; nothing is authorized to spend.
		return Decision{Decision: policy.Deny, Reason: "no active policy for agent"}, nil
	}
	dec, err := p.engine.Evaluate(ctx, pol, req)
	if err != nil {
		return Decision{}, err
	}

	out := Decision{Decision: dec.Decision, FiredRules: dec.FiredRules, Reason: dec.Reason}
	if dec.Decision == policy.NeedsApproval {
		if rule, ok := firstApprovalRule(pol, dec.FiredRules); ok {
			ar, _ := p.approvals.Create(ctx, rule, req)
			out.ApprovalRequestID = ar.ID
		}
	}
	return out, nil
}

// PayInput is Authorize plus an optional approval id that unlocks a previously
// needs_approval quote.
type PayInput struct {
	AuthorizeInput
	ApprovalRequestID string
}

// PayResult is the outcome of a paid call.
type PayResult struct {
	Decision      Decision        `json:"decision"`
	Paid          bool            `json:"paid"`
	Status        int             `json:"status,omitempty"`
	TransactionID string          `json:"transactionId,omitempty"`
	ReceiptID     string          `json:"receiptId,omitempty"`
	Body          json.RawMessage `json:"body,omitempty"`
}

// Pay authorizes and, if allowed, constructs a signed payment and settles it by
// invoking the quote's resource through the seller. A needs_approval quote is
// paid only when a matching approved ApprovalRequestID is supplied. Spend is
// recorded on success so budget/velocity rules see it.
func (p *Plane) Pay(ctx context.Context, in PayInput) (PayResult, error) {
	dec, err := p.Authorize(ctx, in.AuthorizeInput)
	if err != nil {
		return PayResult{}, err
	}

	// Resolve a needs_approval into allow if a matching approval was granted.
	if dec.Decision == policy.NeedsApproval && in.ApprovalRequestID != "" {
		if ar, ok := p.approvals.Get(ctx, in.ApprovalRequestID); ok &&
			ar.DecisionAction() == policy.Allow && ar.AgentID == in.AgentID &&
			ar.Amount == amountOf(in.Quote) {
			dec.Decision = policy.Allow
		}
	}
	if dec.Decision != policy.Allow {
		return PayResult{Decision: dec}, nil
	}

	a, _ := p.agent(in.AgentID)
	client := &buyer.Client{
		AgentID: a.ID, Wallet: a.Wallet, PrivateKey: a.priv, FacilitatorPubKey: p.facPub,
	}
	resp, err := client.Pay(in.Quote)
	if err != nil {
		return PayResult{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	res := PayResult{
		Decision: dec, Status: resp.StatusCode,
		TransactionID: resp.Header.Get("X-Tollgate-Transaction"),
		ReceiptID:     resp.Header.Get("X-Tollgate-Receipt"),
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		res.Paid = true
		res.Body = asJSON(body)
		// Record the charge so budgets and velocity reflect it.
		_ = p.tracker.Record(ctx, policy.Event{
			AgentID: in.AgentID, TaskID: in.TaskID, Currency: in.Quote.Currency,
			Amount: amountOf(in.Quote), At: p.now().UTC(),
		})
	}
	return res, nil
}

// ResolveApproval approves or denies a parked approval request.
func (p *Plane) ResolveApproval(ctx context.Context, id string, approve bool) (policy.ApprovalRequest, bool) {
	return p.approvals.Resolve(ctx, id, approve)
}

// --- helpers ---

func (p *Plane) agent(id string) (*Agent, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.agents[id]
	return a, ok
}

func (p *Plane) policyRequest(ctx context.Context, a *Agent, in AuthorizeInput) (policy.Request, error) {
	host := ""
	if u, err := url.Parse(in.Quote.Resource); err == nil {
		host = u.Hostname() // host without port; allowlists match on host
	}
	bal, err := p.fac.Balance(ctx, a.Wallet, in.Quote.Currency)
	if err != nil {
		return policy.Request{}, err
	}
	return policy.Request{
		AgentID: a.ID, TaskID: in.TaskID, ServiceID: in.Quote.ServiceID,
		Amount: amountOf(in.Quote), Currency: in.Quote.Currency,
		ResourceHost: host, ServiceCategory: in.ServiceCategory,
		Balance: bal, MedianPrice: in.MedianPrice, Now: p.now().UTC(),
	}, nil
}

func firstApprovalRule(pol policy.Policy, fired []string) (policy.Rule, bool) {
	firedSet := make(map[string]bool, len(fired))
	for _, id := range fired {
		firedSet[id] = true
	}
	for _, r := range pol.Rules {
		if firedSet[r.ID] && (r.Type == policy.TypeApproval || r.Type == policy.TypeAnomaly) {
			return r, true
		}
	}
	return policy.Rule{}, false
}

func amountOf(q x402.Quote) int64 {
	n, _ := strconv.ParseInt(q.Amount, 10, 64)
	return n
}

// asJSON returns body as raw JSON if valid, otherwise a JSON string.
func asJSON(body []byte) json.RawMessage {
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	quoted, _ := json.Marshal(string(body))
	return quoted
}

func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("buyerplane: crypto/rand failed: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
