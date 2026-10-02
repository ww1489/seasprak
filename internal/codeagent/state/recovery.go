package state

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
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

func (v View) EffectiveObservation(callID string) (ObservationRevision, bool) {
	var latest ObservationRevision
	for _, reconciliation := range v.Reconciliations {
		if reconciliation.CallID != callID {
			continue
		}
		observation, ok := v.Observations[reconciliation.NewObservationID]
		if ok && observation.Version > latest.Version {
			latest = observation
		}
	}
	return latest, latest.Version != 0
}

// ReconciliationUnresolved checks the latest reconciliation and any later
// evidence without erasing the original observation or a contradictory fact.
func (v View) ReconciliationUnresolved(callID string) bool {
	var latest Reconciliation
	var effective ObservationRevision
	for _, result := range v.Reconciliations {
		if result.CallID != callID {
			continue
		}
		if observation, ok := v.Observations[result.NewObservationID]; ok && observation.Version > effective.Version {
			effective, latest = observation, result
		}
	}
	if effective.Version == 0 {
		return false
	}
	if len(latest.RemainingUnknown) > 0 || len(latest.ConflictRestrictions) > 0 {
		return true
	}
	if after, ok := v.latestObservation(callID); ok && after.Version > effective.Version && after.Observation != effective.Observation {
		return true
	}
	return false
}

func (v View) TraceHasUnresolvedEffects(id string) bool {
	for _, call := range v.Calls {
		if call.Scope.TraceID != id {
			continue
		}
		if v.ReconciliationUnresolved(call.Call.CallID) {
			return true
		}
		if latest, ok := v.EffectiveObservation(call.Call.CallID); ok {
			if unresolvedObservation(latest.Observation) {
				return true
			}
			continue
		}
		if unknownEffect(call) || call.Claimed && call.Observation == nil {
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
		if v.ReconciliationUnresolved(call.Call.CallID) {
			return true
		}
		if latest, ok := v.EffectiveObservation(call.Call.CallID); ok {
			if unresolvedObservation(latest.Observation) {
				return true
			}
			continue
		}
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

func unresolvedObservation(observation agent.ToolObservation) bool {
	return observation.SideEffect == "unknown" || observation.Status == "outcome_unknown"
}

func unknownEffect(call agent.ToolRecord) bool {
	return call.Observation != nil && unresolvedObservation(*call.Observation)
}
