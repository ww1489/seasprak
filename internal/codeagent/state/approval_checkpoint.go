package state

import (
	"context"
	"reflect"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// CommitToolWaitCheckpoint saves only original-call recovery locations and the
// stopped proof. No interaction or approval permission is persisted.
func (m *Manager) CommitToolWaitCheckpoint(ctx context.Context, traceID, pauseID string, cp CheckpointRef, targets map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commitRuntimeApprovalCheckpoint(ctx, traceID, cp, targets, pauseID)
}

// ClaimRuntimeApprovedTool commits only the original call intent and its budget.
// The session mailbox owns the one-time permission and consumes it after this
// atomic commit succeeds; replay can never manufacture that permission.
func (m *Manager) ClaimRuntimeApprovedTool(ctx context.Context, frozen agent.FrozenCall, usage agent.Usage, executionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	call := m.view.Calls[frozen.CallID]
	tr := m.view.Traces[call.Scope.TraceID]
	segment, resumed := m.view.ResumedExecutions[executionID]
	cp, saved := m.view.Checkpoints[segment.CheckpointID]
	if tr == nil || tr.ExecutionID != executionID || tr.ExecutionStopped || !resumed || !saved || segment.DirectResumeID != "" || !containsID(cp.CallIDs, frozen.CallID) || !approvalCheckpointCall(m.view, cp, call) {
		return product.NewError(product.CodePermissionDenied, "runtime claim is not an original resumed tool")
	}
	return m.claimTool(ctx, frozen, usage, true)
}

func (m *Manager) commitRuntimeApprovalCheckpoint(ctx context.Context, traceID string, cp CheckpointRef, targets map[string]string, pauseID string) error {
	tr := m.view.Traces[traceID]
	if tr == nil || tr.State != "running" || !tr.Started || tr.Settled || cp.ID == "" || cp.BlobHash == "" || cp.BlobSize <= 0 || cp.Scope.SessionID != m.sessionID || cp.Scope.BranchID != m.view.BranchID || cp.Scope.TraceID != traceID || cp.Scope.InvocationID != tr.InvocationID || cp.Scope.Generation != tr.Generation || cp.Target != tr.Target || cp.HistoryCommit != m.view.LastSeq || cp.ProjectionRevision != cp.HistoryCommit || cp.LeafID != m.view.LeafID || len(cp.ApprovalTargets) == 0 || len(cp.InteractionIDs) != 0 || !reflect.DeepEqual(cp.ApprovalTargets, targets) || m.view.HasUnresolvedEffects() {
		return product.NewError(product.CodeStateConflict, "approval checkpoint association is invalid")
	}
	seen := make(map[string]bool)
	for callID, target := range cp.ApprovalTargets {
		call, exists := m.view.Calls[callID]
		frozen, saved := m.view.FrozenExecutions["execution:"+callID]
		hash, err := frozen.Digest()
		if !exists || !saved || err != nil || hash != frozen.Hash || frozen.Scope != call.Scope || frozen.RequestedGrantRef == "" || target == "" || seen[target] || call.Claimed || call.Observation != nil || !approvalCheckpointCall(m.view, cp, call) || !containsID(cp.CallIDs, callID) || !containsID(cp.UnfinishedTurnIDs, call.Scope.TurnID) {
			return product.NewError(product.CodeStateConflict, "interrupt target is not an original pending call")
		}
		seen[target] = true
	}
	next := *tr
	next.State, next.ExecutionID, next.CheckpointID = "paused", cp.Scope.ExecutionID, cp.ID
	next.ExecutionStopped, next.Settled = true, false
	controls := []store.Record{record("checkpoint_ref", cp.ID, cp), record("trace", traceID, next)}
	if pauseID != "" {
		op, exists := m.view.Operations[pauseID]
		if !exists || op.Kind != "pause" || op.Receipt.Target != traceID || op.State != "accepted" {
			return product.NewError(product.CodeStateConflict, "approval pause operation is not accepted")
		}
		op.State, op.ResultRef = "completed", cp.ID
		op.Revision++
		controls = append(controls, record("operation", pauseID, op))
	}
	_, err := m.commit(ctx, controls, nil, []agent.Event{m.event("trace.state_changed", traceID, cp.Scope.TurnID, next)})
	return err
}
