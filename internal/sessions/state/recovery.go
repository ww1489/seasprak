package state

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// ConfirmExecutionStopped records the coordinator's proof that the execution
// has exited. Opening an interrupted journal must never manufacture this proof.
// Stopped execution does not imply that its external effects are known.
func (m *Manager) ConfirmExecutionStopped(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[id]
	if tr == nil {
		return product.NewError(product.CodeNotFound, "trace not found")
	}
	if !tr.Started || terminal(tr.State) {
		return product.NewError(product.CodeStateConflict, "trace has no unfinished execution")
	}
	if tr.ExecutionStopped {
		return nil
	}
	next := *tr
	next.ExecutionStopped = true
	_, err := m.commit(ctx, []store.Record{record("trace", id, next)}, nil, nil)
	return err
}

// TraceHasUnresolvedEffects includes an occupied call without a durable result.
// In particular, absence of an in-memory worker is not stopped-execution proof.
func (v View) TraceHasUnresolvedEffects(id string) bool {
	for _, call := range v.Calls {
		if call.Scope.TraceID == id && (unknownEffect(call) || call.Claimed && call.Observation == nil) {
			return true
		}
	}
	return false
}

// HasUnresolvedEffects blocks further execution while unknown effects remain,
// even after their trace becomes terminal. A normal running claim is not yet an
// unknown result and does not prevent accepting work into the existing queue.
func (v View) HasUnresolvedEffects() bool {
	for _, call := range v.Calls {
		if unknownEffect(call) {
			return true
		}
		if call.Claimed && call.Observation == nil {
			tr := v.Traces[call.Scope.TraceID]
			if tr == nil || tr.State != "running" {
				return true
			}
		}
	}
	return false
}

func unknownEffect(call agent.ToolRecord) bool {
	return call.Observation != nil && (call.Observation.SideEffect == "unknown" || call.Observation.Status == "outcome_unknown")
}
