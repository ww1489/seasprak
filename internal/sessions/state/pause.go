package state

import (
	"context"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// CommitPause publishes the stopped proof, immutable checkpoint, and completed
// operation in one journal commit. An unassociated blob is never a recovery point.
func (m *Manager) CommitPause(ctx context.Context, traceID, operationID string, ref CheckpointRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[traceID]
	op, ok := m.view.Operations[operationID]
	if tr == nil || tr.State != "running" || !tr.Started || !ok || op.Kind != "pause" || op.Receipt.Target != traceID || op.State != "accepted" || ref.ID == "" || ref.Scope.TraceID != traceID || ref.Scope.InvocationID != tr.InvocationID || m.view.TraceHasUnresolvedEffects(traceID) {
		return product.NewError(product.CodeStateConflict, "pause association is invalid")
	}
	next := *tr
	next.State = "paused"
	next.ExecutionStopped = true
	next.Settled = false
	next.ExecutionID = ref.Scope.ExecutionID
	next.CheckpointID = ref.ID
	op.State = "completed"
	op.Revision++
	op.ResultRef = ref.ID
	_, err := m.commit(ctx, []store.Record{record("checkpoint_ref", ref.ID, ref), record("trace", traceID, next), record("operation", operationID, op)}, nil, []agent.Event{m.event("trace.state_changed", traceID, "", next)})
	return err
}
