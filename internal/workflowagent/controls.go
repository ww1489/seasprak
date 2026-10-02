package workflowagent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
)

func (w *WorkflowAgent) operationLocked(kind string, cmd WorkflowControlCommand) (operationRecord, bool, error) {
	principal, err := w.principal(cmd.Principal)
	if err != nil {
		return operationRecord{}, false, err
	}
	hash := digest(struct {
		Reason string `json:"reason"`
	}{cmd.Reason})
	if cmd.IdempotencyKey != "" {
		for _, op := range w.state.Operations {
			if op.Kind == kind && op.Key == cmd.IdempotencyKey && op.Principal == principal {
				if op.Digest != hash {
					return op, true, product.NewError(product.CodeIdempotencyConflict, "control key belongs to different content")
				}
				return op, true, nil
			}
		}
	}
	if err := revision(cmd.ExpectedRevision, w.state.Revision); err != nil {
		return operationRecord{}, false, err
	}
	op := operationRecord{Receipt: WorkflowOperationReceipt{OperationID: agent.MustID(), State: "accepted", Target: w.opts.RunID, AcceptedCommit: w.state.Revision + 1, ReceiptScope: "durable"}, Kind: kind, State: "running", Principal: principal, Key: cmd.IdempotencyKey, Digest: hash}
	return op, false, nil
}
func waitSegment(ctx context.Context, frame *segment) error {
	if frame == nil {
		return nil
	}
	select {
	case <-frame.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *WorkflowAgent) Pause(ctx context.Context, cmd WorkflowControlCommand) (WorkflowOperationReceipt, error) {
	return w.stop(ctx, cmd, "pause")
}
func (w *WorkflowAgent) Cancel(ctx context.Context, cmd WorkflowControlCommand) (WorkflowOperationReceipt, error) {
	return w.stop(ctx, cmd, "cancel")
}
func (w *WorkflowAgent) stop(ctx context.Context, cmd WorkflowControlCommand, kind string) (WorkflowOperationReceipt, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowOperationReceipt{}, err
	}
	w.mu.Lock()
	if err := w.writableLocked(); err != nil {
		w.mu.Unlock()
		return WorkflowOperationReceipt{}, err
	}
	op, duplicate, err := w.operationLocked(kind, cmd)
	if err != nil {
		w.mu.Unlock()
		return WorkflowOperationReceipt{}, err
	}
	frame := w.active
	if duplicate {
		w.mu.Unlock()
		return op.Receipt, waitSegment(ctx, frame)
	}
	run := w.state.Run
	if kind == "pause" && (frame == nil || run.State != "running") {
		w.mu.Unlock()
		return WorkflowOperationReceipt{}, product.NewError(product.CodeStateConflict, "workflow has no running segment to pause")
	}
	if kind == "cancel" && terminal(run.State) {
		w.mu.Unlock()
		return WorkflowOperationReceipt{}, product.NewError(product.CodeStateConflict, "workflow already reached a terminal state")
	}
	records := []storage.Record{record("workflow_operation", op.Receipt.OperationID, op)}
	if kind == "pause" {
		run.State = "pausing"
	} else {
		if frame == nil {
			run.State = "cancelled"
			run.ExecutionStopped = true
			records = append(records, w.closeUnclaimedLocked()...)
		} else {
			run.State = "cancelling"
		}
	}
	records = append(records, record("workflow_run", w.opts.RunID, run))
	if frame == nil {
		op.State = "completed"
		records = append(records, record("workflow_operation", op.Receipt.OperationID, op))
	}
	err = w.commitLocked(ctx, records, []agent.Event{w.event("workflow.state_changed", "", map[string]any{"state": run.State})})
	if err != nil {
		w.mu.Unlock()
		return WorkflowOperationReceipt{}, err
	}
	if kind == "cancel" && frame != nil {
		frame.cancel()
	}
	if frame == nil {
		w.clearTransientLocked()
		if terminal(run.State) {
			for _, pending := range w.approvals {
				if pending.decision == "" {
					pending.view.State = "cancelled"
				}
			}
		}
	}
	w.mu.Unlock()
	if err := waitSegment(ctx, frame); err != nil {
		return op.Receipt, err
	}
	w.mu.Lock()
	broken := w.broken
	w.mu.Unlock()
	return op.Receipt, broken
}
func (w *WorkflowAgent) resumeErrorLocked() error {
	if w.active != nil || w.state.Run.State != "paused" || !w.state.Run.ExecutionStopped || w.state.Input == nil {
		return product.NewError(product.CodeIncompatibleResume, "workflow has no safely stopped node recovery point")
	}
	for _, pending := range w.approvals {
		node := w.state.Nodes[pending.view.NodeExecutionID]
		if pending.decision == "" && node.State == "waiting" {
			return product.NewError(product.CodeStateConflict, "workflow approval must be answered before resume")
		}
	}
	if w.state.hasUnknown() {
		return product.NewError(product.CodeReconciliationRequired, "unresolved workflow effects block resume")
	}
	if w.state.Run.ActivityUsed >= w.state.Initial.Limits.ActivityBudget {
		return product.NewError(product.CodeBudgetExhausted, "workflow activity budget exhausted")
	}
	if w.compiled == nil || !w.compiled.Definition.Resumable || w.opts.GenerationFingerprint == "" || w.binding != w.state.Initial.BindingVersion || w.state.RepairRequired {
		return product.NewError(product.CodeIncompatibleResume, "workflow definition or implementation cannot be reconstructed")
	}
	return nil
}
func (w *WorkflowAgent) Resume(ctx context.Context, cmd WorkflowControlCommand) (WorkflowOperationReceipt, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowOperationReceipt{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.writableLocked(); err != nil {
		return WorkflowOperationReceipt{}, err
	}
	op, duplicate, err := w.operationLocked("resume", cmd)
	if err != nil {
		return WorkflowOperationReceipt{}, err
	}
	if duplicate {
		return op.Receipt, nil
	}
	if err := w.resumeErrorLocked(); err != nil {
		return WorkflowOperationReceipt{}, err
	}
	from := w.state.Run.ExecutionID
	run := w.state.Run
	run.State = "running"
	run.ExecutionID = agent.MustID()
	run.ExecutionStopped = false
	run.Result = nil
	run.ErrorCode = ""
	run.FailedNode = ""
	if err := w.commitLocked(ctx, []storage.Record{record("workflow_operation", op.Receipt.OperationID, op), record("workflow_run", w.opts.RunID, run)}, []agent.Event{w.event("workflow.state_changed", "", map[string]any{"state": "running"})}); err != nil {
		return WorkflowOperationReceipt{}, err
	}
	w.startLocked(from)
	return op.Receipt, nil
}
func (w *WorkflowAgent) RequestToolApproval(ctx context.Context, scope agent.ExecutionScope, f agent.FrozenExecution) (*agent.ApprovalWait, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.activeScopeLocked(scope); err != nil {
		return nil, err
	}
	decision, err := w.policyLocked(ctx, f, false)
	if err != nil {
		return nil, err
	}
	if decision != agent.DecisionAsk {
		return nil, product.NewError(product.CodeStateConflict, "tool no longer waits for approval")
	}
	for _, p := range w.approvals {
		if p.frozen.Hash == f.Hash && p.frozen.Scope == f.Scope && p.decision == "" {
			return &agent.ApprovalWait{InteractionID: p.view.ID}, nil
		}
	}
	id := agent.MustID()
	p := &runtimeApproval{view: WorkflowInteraction{ID: id, NodeExecutionID: f.NodeExecutionID, ToolCallID: f.CallID, Question: "Approve this operation once?", Options: []string{"allowed-once", "rejected", "cancelled"}, State: "pending", ExpiresAt: time.Now().Add(config.ApprovalValidity), InstanceID: w.instance}, frozen: f.Clone()}
	w.approvals[id] = p
	p.requestedExecution = w.active.scope.ExecutionID
	raw, _ := json.Marshal(p.view)
	seq := uint64(1)
	w.publishLocked(agent.Event{SchemaVersion: 1, Type: "interaction.requested", Scope: w.eventScope(f.NodeExecutionID), StreamID: w.instance + ":" + id, ChunkSeq: &seq, OccurredAt: time.Now().UTC(), Payload: raw})
	return &agent.ApprovalWait{InteractionID: id}, nil
}
func (w *WorkflowAgent) RespondInteraction(ctx context.Context, response WorkflowInteractionResponse) (WorkflowOperationReceipt, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowOperationReceipt{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.writableLocked(); err != nil {
		return WorkflowOperationReceipt{}, err
	}
	principal, err := w.principal(response.Principal)
	if err != nil {
		return WorkflowOperationReceipt{}, err
	}
	if principal == "" {
		return WorkflowOperationReceipt{}, product.NewError(product.CodePermissionDenied, "approval requires a trusted principal")
	}
	if response.Decision != "allowed-once" && response.Decision != "rejected" && response.Decision != "cancelled" {
		return WorkflowOperationReceipt{}, product.NewError(product.CodeInvalidArgument, "invalid approval decision")
	}
	hash := digest(struct {
		InteractionID string `json:"interactionId"`
		Decision      string `json:"decision"`
	}{response.InteractionID, response.Decision})
	if response.IdempotencyKey != "" {
		for _, op := range w.approvalOps {
			if op.Principal == principal && op.Key == response.IdempotencyKey {
				if op.Digest != hash {
					return WorkflowOperationReceipt{}, product.NewError(product.CodeIdempotencyConflict, "approval key belongs to another decision")
				}
				return op.Receipt, nil
			}
		}
	}
	if response.ExpectedRevision != w.state.Revision {
		return WorkflowOperationReceipt{}, product.NewError(product.CodeStateConflict, "workflow revision changed")
	}
	p := w.approvals[response.InteractionID]
	if p == nil {
		return WorkflowOperationReceipt{}, product.NewError(product.CodeNotFound, "interaction is unavailable in this instance")
	}
	n := w.state.Nodes[p.view.NodeExecutionID]
	call := w.state.Calls[p.view.ToolCallID]
	f := w.state.Frozen[p.frozen.ID]
	if w.active != nil || w.state.Run.State != "paused" || !w.state.Run.ExecutionStopped || n.State != "waiting" || n.ToolCallID != p.view.ToolCallID || call.Claimed || call.Observation != nil || p.decision != "" || p.stoppedExecution != w.state.Run.ExecutionID {
		return WorkflowOperationReceipt{}, product.NewError(product.CodeStateConflict, "interaction has no waiting stopped node")
	}
	if f.Hash != p.frozen.Hash || f.Scope != p.frozen.Scope || w.state.Initial.Policy.Ref != f.PolicyRef || w.state.Initial.Policy.ApprovalPolicy == "never" || w.state.Initial.Policy.SandboxMode == "read-only" && f.Effect != "read" && f.Effect != "none" {
		return WorkflowOperationReceipt{}, product.NewError(product.CodePermissionDenied, "approval differs from original frozen call or policy")
	}
	now := time.Now()
	if !now.Before(p.view.ExpiresAt) || now.Before(p.view.ExpiresAt.Add(-config.ApprovalValidity)) {
		return WorkflowOperationReceipt{}, product.NewError(product.CodePermissionDenied, "approval expired")
	}
	receipt := WorkflowOperationReceipt{OperationID: agent.MustID(), State: "accepted", Target: p.view.ID, ReceiptScope: "instance", InstanceID: w.instance}
	p.decision = response.Decision
	p.view.State = "decided"
	w.approvalOps[receipt.OperationID] = WorkflowOperation{Receipt: receipt, Kind: "respond_interaction", State: "completed", Principal: principal, Key: response.IdempotencyKey, Digest: hash}
	raw, _ := json.Marshal(map[string]string{"interactionId": p.view.ID, "decision": response.Decision})
	seq := uint64(2)
	w.publishLocked(agent.Event{SchemaVersion: 1, Type: "interaction.resolved", Scope: w.eventScope(n.ID), StreamID: w.instance + ":" + p.view.ID, ChunkSeq: &seq, OccurredAt: time.Now().UTC(), Payload: raw})
	return receipt, nil
}

// Close arranges a safe stop independently of the caller's wait deadline. The
// backend stays open until the actual execution exit, even if the caller leaves.
func (w *WorkflowAgent) Close(ctx context.Context) error {
	w.mu.Lock()
	if !w.closing && !w.closed {
		w.closing = true
		frame := w.active
		if frame != nil {
			run := w.state.Run
			if run.State == "running" {
				run.State = "pausing"
				w.closeCause = w.commitLocked(context.Background(), []storage.Record{record("workflow_run", w.opts.RunID, run)}, []agent.Event{w.event("workflow.state_changed", "", map[string]any{"state": "pausing"})})
			}
			frame.cancel()
		}
		go w.closeAfter(frame)
	}
	done := w.closeDone
	w.mu.Unlock()
	select {
	case <-done:
		w.mu.Lock()
		err := w.closeErr
		w.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *WorkflowAgent) closeAfter(frame *segment) {
	if frame != nil {
		<-frame.done
	}
	w.mu.Lock()
	for sub := range w.subs {
		sub.finish(nil)
	}
	w.subs = map[*WorkflowSubscription]struct{}{}
	w.approvals = map[string]*runtimeApproval{}
	w.approvalOps = map[string]WorkflowOperation{}
	w.clearTransientLocked()
	w.mu.Unlock()
	err := w.store.Close()
	w.mu.Lock()
	w.closeCause = errors.Join(w.closeCause, err, w.broken)
	switch {
	case w.closeCause == nil:
		w.closeErr = nil
	case errors.Is(w.closeCause, context.Canceled):
		w.closeErr = context.Canceled
	case errors.Is(w.closeCause, context.DeadlineExceeded):
		w.closeErr = context.DeadlineExceeded
	default:
		w.closeErr = product.NewError(product.CodeStorageUnavailable, "workflow close failed")
	}
	w.closed = true
	close(w.closeDone)
	w.mu.Unlock()
}
