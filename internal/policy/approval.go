package policy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// ApprovalStatus is the state of a human-approval request.
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalDenied   ApprovalStatus = "denied"
	ApprovalExpired  ApprovalStatus = "expired"
)

// ApprovalRequest parks a needs_approval decision until a human resolves it or
// it times out. Approvals never sit on the engine's hot path — the agent's call
// is held, the engine is not.
type ApprovalRequest struct {
	ID        string         `json:"id"`
	AgentID   string         `json:"agentId"`
	TaskID    string         `json:"taskId"`
	ServiceID string         `json:"serviceId"`
	Amount    int64          `json:"amount"`
	Currency  string         `json:"currency"`
	RuleID    string         `json:"ruleId"`
	Status    ApprovalStatus `json:"status"`
	CreatedAt time.Time      `json:"createdAt"`
	ExpiresAt time.Time      `json:"expiresAt"`
	OnTimeout Action         `json:"onTimeout"`
}

// ApprovalManager creates, resolves and expires approval requests, firing the
// policy's approval webhook when a request is created.
type ApprovalManager struct {
	mu    sync.Mutex
	byID  map[string]*ApprovalRequest
	hc    *http.Client
	now   func() time.Time
	newID func() string
}

// NewApprovalManager returns a manager with real time and a random id source.
func NewApprovalManager() *ApprovalManager {
	return &ApprovalManager{
		byID:  make(map[string]*ApprovalRequest),
		hc:    &http.Client{Timeout: 10 * time.Second},
		now:   time.Now,
		newID: randomID,
	}
}

// Create records a pending approval for a needs_approval decision and fires the
// rule's webhook (best-effort). rule must be a TypeApproval rule.
func (m *ApprovalManager) Create(ctx context.Context, rule Rule, req Request) (ApprovalRequest, error) {
	timeout := 5 * time.Minute
	if rule.Timeout != "" {
		if d, err := time.ParseDuration(rule.Timeout); err == nil {
			timeout = d
		}
	}
	onTimeout := rule.OnTimeout
	if onTimeout != Allow {
		onTimeout = Deny // default and safe posture
	}

	now := m.now().UTC()
	ar := &ApprovalRequest{
		ID:        m.newID(),
		AgentID:   req.AgentID,
		TaskID:    req.TaskID,
		ServiceID: req.ServiceID,
		Amount:    req.Amount,
		Currency:  req.Currency,
		RuleID:    rule.ID,
		Status:    ApprovalPending,
		CreatedAt: now,
		ExpiresAt: now.Add(timeout),
		OnTimeout: onTimeout,
	}
	m.mu.Lock()
	m.byID[ar.ID] = ar
	m.mu.Unlock()

	if rule.Webhook != "" {
		m.fireWebhook(ctx, rule.Webhook, *ar)
	}
	return *ar, nil
}

// Resolve approves or denies a pending request. Terminal requests are returned
// unchanged (idempotent).
func (m *ApprovalManager) Resolve(_ context.Context, id string, approve bool) (ApprovalRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ar, ok := m.byID[id]
	if !ok {
		return ApprovalRequest{}, false
	}
	m.expireLocked(ar)
	if ar.Status == ApprovalPending {
		if approve {
			ar.Status = ApprovalApproved
		} else {
			ar.Status = ApprovalDenied
		}
	}
	return *ar, true
}

// Get returns a request with expiry applied.
func (m *ApprovalManager) Get(_ context.Context, id string) (ApprovalRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ar, ok := m.byID[id]
	if !ok {
		return ApprovalRequest{}, false
	}
	m.expireLocked(ar)
	return *ar, true
}

// Decision maps a request's resolved status to a policy Action. A pending or
// expired request yields the rule's on_timeout action (until resolved).
func (ar ApprovalRequest) DecisionAction() Action {
	switch ar.Status {
	case ApprovalApproved:
		return Allow
	case ApprovalDenied:
		return Deny
	case ApprovalExpired:
		return ar.OnTimeout
	default: // pending
		return NeedsApproval
	}
}

func (m *ApprovalManager) expireLocked(ar *ApprovalRequest) {
	if ar.Status == ApprovalPending && m.now().After(ar.ExpiresAt) {
		ar.Status = ApprovalExpired
	}
}

func (m *ApprovalManager) fireWebhook(ctx context.Context, url string, ar ApprovalRequest) {
	body, _ := json.Marshal(map[string]any{
		"approvalRequestId": ar.ID,
		"agentId":           ar.AgentID,
		"taskId":            ar.TaskID,
		"serviceId":         ar.ServiceID,
		"amount":            ar.Amount,
		"currency":          ar.Currency,
		"ruleId":            ar.RuleID,
		"expiresAt":         ar.ExpiresAt,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return // best-effort; the request is parked regardless
	}
	resp.Body.Close()
}

func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("policy: crypto/rand failed: " + err.Error())
	}
	return "apr_" + hex.EncodeToString(b[:])
}
