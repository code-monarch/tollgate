package registry

import (
	"context"
	"math"
	"strconv"
)

// stats holds the reputation signals for one service.
type stats struct {
	calls       int64
	disputes    int64
	slaBreaches int64
}

// score derives a 0..5 reputation from the signals and the advertised SLA.
//
//	dispute rate  = disputes / calls          (lower is better)
//	sla adherence = 1 - slaBreaches / calls    (higher is better)
//	score = ( 0.7*(1-disputeRate) + 0.3*slaAdherence ) * 5
//
// With no calls yet, reputation seeds from the advertised uptime so brand-new
// services aren't unrankable. Explainable and cheap — no ML on any path.
func (s *stats) score(sla SLA) float64 {
	if s.calls == 0 {
		return round2(clamp01(sla.Uptime) * 5)
	}
	calls := float64(s.calls)
	disputeRate := float64(s.disputes) / calls
	slaAdherence := 1 - float64(s.slaBreaches)/calls
	raw := 0.7*(1-disputeRate) + 0.3*clamp01(slaAdherence)
	return round2(clamp01(raw) * 5)
}

// RecordOutcome updates a service's reputation signals after a call completes.
// disputed marks a disputed/refunded transaction; slaBreached marks an SLA miss
// (timeout, error, latency breach). Reputation is recomputed immediately.
func (m *MemStore) RecordOutcome(ctx context.Context, serviceID string, disputed, slaBreached bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.stats[serviceID]
	if !ok {
		st = &stats{}
		m.stats[serviceID] = st
	}
	st.calls++
	if disputed {
		st.disputes++
	}
	if slaBreached {
		st.slaBreaches++
	}
	if svc, ok := m.byID[serviceID]; ok {
		svc.Reputation = st.score(svc.SLA)
		m.byID[serviceID] = svc
	}
	return nil
}

func parseMinor(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
