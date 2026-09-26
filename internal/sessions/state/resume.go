package state

import (
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

type ResumedExecution struct {
	ID           string               `json:"id"`
	CheckpointID string               `json:"checkpointId"`
	OperationID  string               `json:"operationId"`
	Scope        agent.ExecutionScope `json:"scope"`
}

func applyResumedExecution(v *View, record store.Record) error {
	var segment ResumedExecution
	if err := json.Unmarshal(record.Payload, &segment); err != nil {
		return err
	}
	cp, ok := v.Checkpoints[segment.CheckpointID]
	op, accepted := v.Operations[segment.OperationID]
	expected := cp.Scope
	expected.ExecutionID = segment.ID
	if !ok || !accepted || op.Kind != "resume" || op.Receipt.Target != cp.Scope.TraceID || op.Receipt.AcceptedCommit != v.LastSeq+1 || segment.ID == cp.Scope.ExecutionID || segment.Scope != expected {
		return product.NewError(product.CodeIncompatibleVersion, "invalid resumed execution mapping")
	}
	return putImmutable(&v.ResumedExecutions, record.ID, segment.ID, segment)
}

// CommitResume consumes the current checkpoint and records acceptance and the
// new execution segment atomically. It preserves the original invocation and
// budgets. A crashed accepted Resume cannot reuse the consumed old checkpoint.
func (m *Manager) CommitResume(ctx context.Context, cmd OperationCommand, checkpointID, executionID string) (OperationReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if receipt, found, err := m.findOperation(cmd); found || err != nil {
		return receipt, err
	}
	if cmd.Kind != "resume" || cmd.ExpectedRevision != m.view.LastSeq {
		return OperationReceipt{}, product.NewError(product.CodeStateConflict, "invalid resume operation or revision")
	}
	cp, ok := m.view.Checkpoints[checkpointID]
	tr := m.view.Traces[cp.Scope.TraceID]
	if !ok || tr == nil || cmd.Target != tr.ID || tr.State != "paused" || !tr.Started || tr.Settled || !tr.ExecutionStopped || tr.CheckpointID != cp.ID || tr.ExecutionID != cp.Scope.ExecutionID || executionID == "" || executionID == tr.ExecutionID || cp.Scope.InvocationID != tr.InvocationID || m.view.HasUnresolvedEffects() {
		return OperationReceipt{}, product.NewError(product.CodeIncompatibleResume, "checkpoint is not the current stopped execution")
	}
	receipt := OperationReceipt{OperationID: agent.MustID(), State: "accepted", Target: tr.ID, AcceptedCommit: m.view.LastSeq + 1}
	digest, err := operationDigest(cmd)
	if err != nil {
		return OperationReceipt{}, err
	}
	op := Operation{Receipt: receipt, Principal: cmd.Principal, SessionID: m.sessionID, Kind: "resume", Key: cmd.IdempotencyKey, Digest: digest, Revision: 1, State: "accepted"}
	next := *tr
	next.State, next.ExecutionID, next.CheckpointID = "running", executionID, ""
	next.ExecutionStopped = false
	scope := cp.Scope
	scope.ExecutionID = executionID
	segment := ResumedExecution{ID: executionID, CheckpointID: cp.ID, OperationID: receipt.OperationID, Scope: scope}
	_, err = m.commit(ctx, []store.Record{record("operation", receipt.OperationID, op), record("resumed_execution", executionID, segment), record("trace", tr.ID, next)}, nil, []agent.Event{m.event("trace.state_changed", tr.ID, cp.Scope.TurnID, next)})
	if err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
