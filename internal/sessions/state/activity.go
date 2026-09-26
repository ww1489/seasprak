package state

import (
	"context"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"time"
)

// ActivityBudget separates measured execution from crash-conservative occupancy.
// No wall-clock timestamp is persisted: stopped and paused time is never billed.
type ActivityBudget struct {
	Known       bool          `json:"known"`
	Unknown     bool          `json:"unknown,omitempty"`
	Settled     time.Duration `json:"settled"`
	Reserved    time.Duration `json:"reserved"`
	Uncertain   time.Duration `json:"uncertain"`
	ExecutionID string        `json:"executionId,omitempty"`
	Revision    uint64        `json:"revision"`
}

func (m *Manager) ReserveActivity(ctx context.Context, trace, execution string, revision uint64, elapsed time.Duration) (ActivityBudget, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[trace]
	if tr == nil || tr.State != "running" || execution == "" {
		return ActivityBudget{}, product.NewError(product.CodeStateConflict, "activity trace is not running")
	}
	a := tr.Activity
	if !a.Known || a.Unknown {
		return ActivityBudget{}, product.NewError(product.CodeReconciliationRequired, "historic activity usage is unknown")
	}
	if a.Revision != revision || elapsed < 0 || elapsed > a.Reserved || (a.Reserved > 0 && a.ExecutionID != execution) {
		return ActivityBudget{}, product.NewError(product.CodeStateConflict, "activity reservation changed")
	}
	a.Settled += elapsed
	remaining := tr.Limits.ActivityBudget - a.Settled - a.Uncertain
	if remaining <= 0 {
		return ActivityBudget{}, product.NewError(product.CodeBudgetExhausted, "activity budget exhausted")
	}
	a.Reserved = min(time.Second, remaining)
	a.ExecutionID = execution
	a.Revision++
	next := *tr
	next.Activity = a
	_, err := m.commit(ctx, []store.Record{record("trace", trace, next)}, nil, nil)
	if err != nil {
		return ActivityBudget{}, err
	}
	return a, nil
}
func (m *Manager) SettleActivity(ctx context.Context, trace, execution string, revision uint64, elapsed time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[trace]
	if tr == nil {
		return product.NewError(product.CodeNotFound, "activity trace not found")
	}
	a := tr.Activity
	if a.Revision != revision || a.ExecutionID != execution || a.Reserved <= 0 || elapsed < 0 {
		return product.NewError(product.CodeStateConflict, "activity reservation changed")
	}
	// Only the coordinator, after real exit, supplies elapsed. Uncooperative work
	// may exceed its reservation: preserve that measured overrun, never hide it.
	a.Settled += elapsed
	a.Reserved = 0
	a.ExecutionID = ""
	a.Revision++
	next := *tr
	next.Activity = a
	_, err := m.commit(ctx, []store.Record{record("trace", trace, next)}, nil, nil)
	return err
}
