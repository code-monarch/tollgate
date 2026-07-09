package policy

import (
	"context"
	"sync"
	"time"
)

// Event is one authorized charge, recorded so budget and velocity rules can see
// prior spend within a window.
type Event struct {
	AgentID  string
	TaskID   string
	Currency string
	Amount   int64
	At       time.Time
}

// Tracker answers the spend/velocity questions budget and velocity rules ask.
// It reflects authorized charges; the buyer plane records an Event when a
// payment is authorized.
type Tracker interface {
	TaskSpend(ctx context.Context, agentID, taskID, currency string) (int64, error)
	AgentSpendSince(ctx context.Context, agentID, currency string, since time.Time) (int64, error)
	AgentCountSince(ctx context.Context, agentID string, since time.Time) (int, error)
	Record(ctx context.Context, e Event) error
}

// MemTracker is an in-memory Tracker.
type MemTracker struct {
	mu     sync.Mutex
	events []Event
}

// NewMemTracker returns an empty tracker.
func NewMemTracker() *MemTracker { return &MemTracker{} }

// Record appends an authorized charge.
func (m *MemTracker) Record(_ context.Context, e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return nil
}

// TaskSpend sums spend for an agent's task, all-time within the task.
func (m *MemTracker) TaskSpend(_ context.Context, agentID, taskID, currency string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int64
	for _, e := range m.events {
		if e.AgentID == agentID && e.TaskID == taskID && e.Currency == currency {
			total += e.Amount
		}
	}
	return total, nil
}

// AgentSpendSince sums an agent's spend at or after since.
func (m *MemTracker) AgentSpendSince(_ context.Context, agentID, currency string, since time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int64
	for _, e := range m.events {
		if e.AgentID == agentID && e.Currency == currency && !e.At.Before(since) {
			total += e.Amount
		}
	}
	return total, nil
}

// AgentCountSince counts an agent's charges at or after since.
func (m *MemTracker) AgentCountSince(_ context.Context, agentID string, since time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int
	for _, e := range m.events {
		if e.AgentID == agentID && !e.At.Before(since) {
			n++
		}
	}
	return n, nil
}
