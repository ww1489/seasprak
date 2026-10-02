package state

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// CommitApprovalCheckpoint is a legacy journal fixture writer, not production.
func (m *Manager) CommitApprovalCheckpoint(ctx context.Context, traceID string, cp CheckpointRef, targets map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[traceID]
	if tr == nil || tr.State != "running" || !tr.Started || tr.Settled || cp.ID == "" || cp.BlobHash == "" || cp.BlobSize <= 0 || cp.Scope.SessionID != m.sessionID || cp.Scope.BranchID != m.view.BranchID || cp.Scope.TraceID != traceID || cp.Scope.InvocationID != tr.InvocationID || cp.Scope.Generation != tr.Generation || cp.Target != tr.Target || cp.HistoryCommit != m.view.LastSeq || cp.ProjectionRevision != cp.HistoryCommit || cp.LeafID != m.view.LeafID || len(cp.InteractionIDs) == 0 || len(targets) != len(cp.InteractionIDs) || m.view.HasUnresolvedEffects() {
		return product.NewError(product.CodeStateConflict, "approval checkpoint association is invalid")
	}
	next := *tr
	next.State, next.ExecutionID, next.CheckpointID = "paused", cp.Scope.ExecutionID, cp.ID
	next.ExecutionStopped, next.Settled = true, false
	controls := []store.Record{record("checkpoint_ref", cp.ID, cp), record("trace", traceID, next)}
	var events []agent.Event
	seen, seenTargets := make(map[string]bool), make(map[string]bool)
	for _, id := range cp.InteractionIDs {
		in, exists := m.view.Interactions[id]
		approval := m.view.Approvals[in.ApprovalID]
		_, call, err := approvalDescription(m.view, approval)
		target := targets[id]
		if !exists || err != nil || seen[id] || target == "" || seenTargets[target] || in.Kind != "approval" || in.State != "pending" || approval.State != "asked" || approval.InteractionID != in.ID || call.Claimed || call.Observation != nil || !approvalCheckpointCall(m.view, cp, call) || !containsID(cp.CallIDs, call.Call.CallID) || !containsID(cp.UnfinishedTurnIDs, call.Scope.TurnID) {
			return product.NewError(product.CodeStateConflict, "interrupt target is not an original pending approval")
		}
		seen[id], seenTargets[target] = true, true
		binding := ApprovalBinding{ID: cp.ID + ":" + id, InteractionID: id, ApprovalID: approval.ID, CheckpointID: cp.ID, TargetRef: target}
		controls = append(controls, record("approval_binding", binding.ID, binding))
		events = append(events, m.event("interaction.ready", traceID, in.Scope.TurnID, struct {
			InteractionID string `json:"interactionId"`
		}{id}))
	}
	events = append(events, m.event("trace.state_changed", traceID, cp.Scope.TurnID, next))
	_, err := m.commit(ctx, controls, nil, events)
	return err
}
