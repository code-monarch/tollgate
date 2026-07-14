// Package policy is Tollgate's buyer-side moat: the last gate before a buyer's
// funds move. Given a signed quote and the agent's context it returns
// allow / deny / needs_approval, deterministically and with the rules that
// fired. It is a fixed, audited set of rule types — deliberately not a
// user-scripted DSL (docs/05-policy-engine.md).
package policy

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Action is a policy decision or a rule verdict.
type Action string

const (
	Allow         Action = "allow"
	Deny          Action = "deny"
	NeedsApproval Action = "needs_approval"
)

// Rule types.
const (
	TypeAmountCeiling = "amount_ceiling"
	TypeBudget        = "budget"
	TypeAllowlist     = "allowlist"
	TypeBlocklist     = "blocklist"
	TypeVelocity      = "velocity"
	TypeAnomaly       = "anomaly"
	TypeApproval      = "approval"
	// TypeExhaustRights gates what the agent may ever grant a seller over the
	// intelligence exhaust of a call. Values lists the grantable rights. Absent
	// rule ⇒ nothing is grantable: silence never grants
	// (docs/08-learning-boundary.md).
	TypeExhaustRights = "exhaust_rights"
)

// Rule is one entry in a policy. Fields are a superset across rule types; only
// those relevant to Type are read (see engine.go). Amounts are minor-unit
// strings, never floats.
type Rule struct {
	ID   string `json:"id"`
	Type string `json:"type"`

	Scope  string `json:"scope,omitempty"`  // call | task | agent
	Max    string `json:"max,omitempty"`    // amount_ceiling, budget (minor units)
	Window string `json:"window,omitempty"` // task | 24h | 1h | 1m | Go duration

	Field  string   `json:"field,omitempty"`  // allowlist/blocklist: resource_host | service_category | ...
	Values []string `json:"values,omitempty"` // allowlist/blocklist values (host globs allowed);
	//                                           exhaust_rights: the grantable rights

	MaxCount int `json:"max_count,omitempty"` // velocity

	Signal string  `json:"signal,omitempty"` // anomaly
	Factor float64 `json:"factor,omitempty"` // anomaly: >factor*median → flag

	Threshold string `json:"threshold,omitempty"` // approval (minor units)
	Webhook   string `json:"webhook,omitempty"`
	Timeout   string `json:"timeout,omitempty"`    // Go duration
	OnTimeout Action `json:"on_timeout,omitempty"` // deny | allow
}

// Defaults sets the baseline posture. deny is the safe default.
type Defaults struct {
	Action Action `json:"action"`
}

// Policy is a versioned rule set attached to an agent or org.
type Policy struct {
	Version  int      `json:"version"`
	Currency string   `json:"currency"`
	Defaults Defaults `json:"defaults"`
	Rules    []Rule   `json:"rules"`
}

// Request is the context a decision is made against.
type Request struct {
	AgentID         string
	TaskID          string
	ServiceID       string
	Amount          int64 // quote amount, minor units
	Currency        string
	ResourceHost    string
	ServiceCategory string
	Balance         int64 // available balance, minor units (caller reads from ledger)
	MedianPrice     int64 // median price for this service (anomaly); 0 = unknown
	Now             time.Time

	// RequiredRights are the exhaust rights the seller will not serve without,
	// read from the signed quote. If the policy does not make all of them
	// grantable, the call is denied and nothing crosses the boundary.
	RequiredRights []string
}

// Decision is the engine's output. FiredRules lists every rule that constrained
// the outcome (produced deny or needs_approval), for the audit log.
type Decision struct {
	Decision   Action   `json:"decision"`
	FiredRules []string `json:"firedRules"`
	Reason     string   `json:"reason,omitempty"`
}

// GrantableRights is the union of every exhaust_rights rule's Values: the complete
// set of rights this policy permits the agent to grant a seller, and therefore the
// most the buyer will ever consent to.
//
// A policy with no exhaust_rights rule returns nothing — the boundary is closed by
// default, and a seller that asks for nothing still gets nothing. This is the
// deliberate inverse of the industry default, where usage rights are reserved
// unless the customer opts out (docs/08-learning-boundary.md).
func GrantableRights(p Policy) []string {
	var out []string
	seen := make(map[string]bool)
	for _, r := range p.Rules {
		if r.Type != TypeExhaustRights {
			continue
		}
		for _, v := range r.Values {
			if v != "" && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ---- versioned policy store ----

// Scope identifies whose policy applies.
type Scope struct {
	Type string // agent | org
	ID   string
}

func (s Scope) key() string { return s.Type + ":" + s.ID }

// Store holds versioned policies, one active version per scope.
type Store struct {
	mu       sync.Mutex
	versions map[string][]Policy // scope key -> versions (in order)
	active   map[string]int      // scope key -> active version
}

// NewStore returns an empty policy store.
func NewStore() *Store {
	return &Store{versions: make(map[string][]Policy), active: make(map[string]int)}
}

// Create adds a new immutable policy version for a scope and makes it active.
// The version number is assigned monotonically per scope.
func (s *Store) Create(_ context.Context, scope Scope, p Policy) (Policy, error) {
	if scope.Type != "agent" && scope.Type != "org" {
		return Policy{}, fmt.Errorf("policy: scope type must be agent|org, got %q", scope.Type)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := scope.key()
	p.Version = len(s.versions[k]) + 1
	if p.Defaults.Action == "" {
		p.Defaults.Action = Deny // safe posture
	}
	s.versions[k] = append(s.versions[k], p)
	s.active[k] = p.Version
	return p, nil
}

// Active returns the active policy for a scope.
func (s *Store) Active(_ context.Context, scope Scope) (Policy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := scope.key()
	v, ok := s.active[k]
	if !ok {
		return Policy{}, false
	}
	return s.versions[k][v-1], true
}
