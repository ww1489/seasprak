package sessions

import (
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

// A late original-execution result is evidence, not permission to modify the
// current segment. Append to Step 2's observation chain; keep the original
// ToolRecord, unknown-effect gates, claim, usage and execution state unchanged.
// This is not Reconcile: no release, effective-result projection or Resume.
// Resumed-segment provenance requires Step 13's durable mapping; it must not be
// inferred by accepting an arbitrary different ExecutionID here.
func (rt *runtime) recordLateObservation(ctx context.Context, scope agent.ExecutionScope, fact agent.Fact) error {
	conflict := func() error {
		return product.NewError(product.CodeStateConflict, "late observation identity is not an original claimed execution")
	}
	if fact.Kind != "tool_observation" || scope.ExecutionID == "" {
		return conflict()
	}
	var incoming agent.ToolRecord
	if err := json.Unmarshal(fact.Payload, &incoming); err != nil {
		return err
	}
	original, ok := rt.manager.View().Calls[incoming.Call.CallID]
	if !ok || !original.Claimed || !incoming.Claimed || incoming.Observation == nil || original.Call != incoming.Call || original.Scope != incoming.Scope {
		return conflict()
	}
	if scope.TurnID == "" {
		scope.TurnID = original.Scope.TurnID
	}
	if scope != original.Scope {
		return conflict()
	}
	previous, err := rt.manager.LatestObservation(incoming.Call.CallID)
	if err != nil {
		pe, ok := product.AsError(err)
		if !ok || pe.Code != product.CodeNotFound {
			return err
		}
	}
	if previous.Version > 0 && previous.Observation == *incoming.Observation {
		return nil
	}
	next := state.ObservationRevision{ID: agent.MustID(), CallID: incoming.Call.CallID, Version: previous.Version + 1, PreviousID: previous.ID, Observation: *incoming.Observation}
	return rt.manager.AppendObservation(ctx, previous.Version, next, nil)
}
