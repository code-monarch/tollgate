package rail

import (
	"context"
	"fmt"
	"sync"
)

// Mock is an in-memory Rail for the demo and tests. It records every Send and
// returns a configurable status (default Success), standing in for a live
// provider with no network I/O.
type Mock struct {
	mu     sync.Mutex
	Result Status // status to return from Send (default StatusSuccess)
	sends  []Transfer
	byRef  map[string]Confirmation
	seq    int
}

// NewMock returns a Mock that confirms transfers as Success.
func NewMock() *Mock {
	return &Mock{Result: StatusSuccess, byRef: make(map[string]Confirmation)}
}

// Send implements Rail. It is idempotent on Reference: a repeated reference
// returns the original confirmation.
func (m *Mock) Send(ctx context.Context, t Transfer) (Confirmation, error) {
	if t.Amount <= 0 {
		return Confirmation{}, fmt.Errorf("rail: non-positive amount %d", t.Amount)
	}
	if t.ToAddress == "" {
		return Confirmation{}, fmt.Errorf("rail: empty to_address")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if c, ok := m.byRef[t.Reference]; ok {
		return c, nil
	}
	m.seq++
	status := m.Result
	if status == "" {
		status = StatusSuccess
	}
	c := Confirmation{
		ProviderRef: fmt.Sprintf("mock-rail-%d", m.seq),
		Status:      status,
	}
	m.byRef[t.Reference] = c
	m.sends = append(m.sends, t)
	return c, nil
}

// Sends returns a copy of all recorded transfers (test helper).
func (m *Mock) Sends() []Transfer {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Transfer, len(m.sends))
	copy(out, m.sends)
	return out
}
