package policy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestApproval_ResolveApprove(t *testing.T) {
	m := NewApprovalManager()
	ar, _ := m.Create(context.Background(), Rule{ID: "approve", Type: TypeApproval, Threshold: "1000000"}, baseReq())
	if ar.Status != ApprovalPending || ar.DecisionAction() != NeedsApproval {
		t.Fatalf("new approval should be pending: %+v", ar)
	}

	resolved, ok := m.Resolve(context.Background(), ar.ID, true)
	if !ok || resolved.Status != ApprovalApproved || resolved.DecisionAction() != Allow {
		t.Fatalf("approve should allow: %+v", resolved)
	}
	// Idempotent: re-resolving does not flip it.
	if again, _ := m.Resolve(context.Background(), ar.ID, false); again.Status != ApprovalApproved {
		t.Fatalf("resolved approval should be terminal, got %s", again.Status)
	}
}

func TestApproval_ResolveDeny(t *testing.T) {
	m := NewApprovalManager()
	ar, _ := m.Create(context.Background(), Rule{ID: "approve", Type: TypeApproval}, baseReq())
	resolved, _ := m.Resolve(context.Background(), ar.ID, false)
	if resolved.DecisionAction() != Deny {
		t.Fatalf("denied approval should deny, got %s", resolved.DecisionAction())
	}
}

func TestApproval_TimeoutAppliesOnTimeout(t *testing.T) {
	m := NewApprovalManager()
	now := t0
	m.now = func() time.Time { return now }

	ar, _ := m.Create(context.Background(), Rule{
		ID: "approve", Type: TypeApproval, Timeout: "300s", OnTimeout: Deny,
	}, baseReq())

	// Before expiry: still pending.
	if got, _ := m.Get(context.Background(), ar.ID); got.Status != ApprovalPending {
		t.Fatalf("should be pending before expiry, got %s", got.Status)
	}
	// After expiry: expired → on_timeout deny.
	now = t0.Add(301 * time.Second)
	got, _ := m.Get(context.Background(), ar.ID)
	if got.Status != ApprovalExpired || got.DecisionAction() != Deny {
		t.Fatalf("expired should apply on_timeout deny, got %s / %s", got.Status, got.DecisionAction())
	}
}

func TestApproval_TimeoutAllowPosture(t *testing.T) {
	m := NewApprovalManager()
	now := t0
	m.now = func() time.Time { return now }
	ar, _ := m.Create(context.Background(), Rule{ID: "a", Type: TypeApproval, Timeout: "1s", OnTimeout: Allow}, baseReq())
	now = t0.Add(2 * time.Second)
	if got, _ := m.Get(context.Background(), ar.ID); got.DecisionAction() != Allow {
		t.Fatalf("on_timeout allow should allow after expiry, got %s", got.DecisionAction())
	}
}

func TestApproval_FiresWebhook(t *testing.T) {
	var hits int32
	var gotID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			ApprovalRequestID string `json:"approvalRequestId"`
		}
		json.Unmarshal(body, &payload)
		gotID = payload.ApprovalRequestID
	}))
	defer srv.Close()

	m := NewApprovalManager()
	ar, _ := m.Create(context.Background(), Rule{
		ID: "approve", Type: TypeApproval, Threshold: "1000000", Webhook: srv.URL,
	}, baseReq())

	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("webhook fired %d times, want 1", hits)
	}
	if gotID != ar.ID {
		t.Fatalf("webhook carried id %q, want %q", gotID, ar.ID)
	}
}
