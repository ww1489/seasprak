package state

// These writers exist only in the test binary to construct and validate old
// journal formats. Production cannot persist an approval answer or consumption.
import (
	"context"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"time"
)

func (m *Manager) RequestApproval(ctx context.Context, expected uint64, frozenID, question string, now time.Time) (Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	frozen, exists := m.view.FrozenExecutions[frozenID]
	call := m.view.Calls[frozen.CallID]
	tr := m.view.Traces[call.Scope.TraceID]
	approval := Approval{ID: agent.MustID(), InteractionID: agent.MustID(), CallID: frozen.CallID, Scope: frozen.Scope, FrozenExecutionID: frozenID, FrozenHash: frozen.Hash, GrantRef: frozen.RequestedGrantRef, State: "asked", ExpiresAt: now.UTC().Add(config.ApprovalValidity)}
	if !exists || tr == nil || tr.State != "running" || !tr.Started || tr.Settled || frozen.Scope.SessionID != m.sessionID || frozen.Scope.BranchID != m.view.BranchID || frozen.Scope.Generation != tr.Generation || frozen.Scope.ExecutionID == "" || call.Claimed || call.Observation != nil || question == "" || now.IsZero() {
		return Interaction{}, product.NewError(product.CodeStateConflict, "call is not awaiting an execution approval")
	}
	if _, _, err := approvalDescription(m.view, approval); err != nil {
		return Interaction{}, err
	}
	if err := approvalPolicy(m.view, frozen); err != nil {
		return Interaction{}, err
	}
	for _, old := range m.view.Approvals {
		if old.FrozenExecutionID == frozenID && old.FrozenHash == frozen.Hash && old.Scope == frozen.Scope {
			in := m.view.Interactions[old.InteractionID]
			if in.Question != question || in.ApprovalID != old.ID {
				return Interaction{}, product.NewError(product.CodeStateConflict, "approval question or binding changed")
			}
			return clone(in), nil
		}
	}
	if expected != m.view.LastSeq {
		return Interaction{}, product.NewError(product.CodeStateConflict, "session revision changed")
	}
	in := Interaction{ID: approval.InteractionID, Kind: "approval", Scope: approval.Scope, CallID: approval.CallID, ApprovalID: approval.ID, Question: question, Options: []string{"allowed-once", "rejected", "cancelled"}, State: "pending", ExpiresAt: approval.ExpiresAt}
	_, err := m.commit(ctx, []store.Record{record("interaction", in.ID, in), record("approval", approval.ID, approval)}, nil, []agent.Event{m.event("interaction.asked", in.Scope.TraceID, in.Scope.TurnID, in)})
	if err != nil {
		return Interaction{}, err
	}
	return clone(in), nil
}

func (m *Manager) RespondApproval(ctx context.Context, cmd OperationCommand, decision string, now time.Time) (OperationReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	receipt, found, err := m.findOperation(cmd)
	if err != nil {
		return OperationReceipt{}, err
	}
	if cmd.Kind != "respond_interaction" || cmd.Target == "" || cmd.Principal == "" {
		return OperationReceipt{}, product.NewError(product.CodePermissionDenied, "approval response requires a trusted principal and interaction")
	}
	if err := responseContent(cmd, decision); err != nil {
		return OperationReceipt{}, err
	}
	if found {
		return receipt, nil
	}
	if cmd.ExpectedRevision != m.view.LastSeq {
		return OperationReceipt{}, product.NewError(product.CodeStateConflict, "session revision changed")
	}
	in, exists := m.view.Interactions[cmd.Target]
	approval := m.view.Approvals[in.ApprovalID]
	tr := m.view.Traces[in.Scope.TraceID]
	if !exists || in.Kind != "approval" || approval.InteractionID != in.ID || tr == nil || tr.State != "paused" || !tr.ExecutionStopped || tr.Settled {
		return OperationReceipt{}, product.NewError(product.CodeStateConflict, "interaction has no safely stopped checkpoint")
	}
	bindingID := tr.CheckpointID + ":" + in.ID
	if tr.DirectResumeID != "" {
		bindingID = tr.DirectResumeID + ":" + in.ID
	}
	binding, bound := m.view.ApprovalBindings[bindingID]
	if !bound || binding.ApprovalID != approval.ID || !validApprovalBinding(m.view, binding) {
		return OperationReceipt{}, product.NewError(product.CodeStateConflict, "interaction is not associated with the current checkpoint")
	}
	if _, decided := m.view.ApprovalDecisions[approval.ID]; decided {
		return OperationReceipt{}, product.NewError(product.CodeStateConflict, "approval already has a decision")
	}
	if err := approvalTime(approval, now); err != nil {
		return OperationReceipt{}, err
	}
	frozen, call, err := approvalDescription(m.view, approval)
	if err != nil {
		return OperationReceipt{}, err
	}
	if call.Claimed || call.Observation != nil || approval.State != "asked" {
		return OperationReceipt{}, product.NewError(product.CodeStateConflict, "approval call is no longer pending")
	}
	if err := approvalPolicy(m.view, frozen); err != nil {
		return OperationReceipt{}, err
	}
	digest, err := operationDigest(cmd)
	if err != nil {
		return OperationReceipt{}, err
	}
	receipt = OperationReceipt{OperationID: agent.MustID(), State: "accepted", Target: in.ID, AcceptedCommit: m.view.LastSeq + 1}
	op := Operation{Receipt: receipt, Principal: cmd.Principal, SessionID: m.sessionID, Kind: cmd.Kind, Key: cmd.IdempotencyKey, Digest: digest, Revision: 1, State: "accepted"}
	answer := ApprovalDecision{ApprovalID: approval.ID, InteractionID: in.ID, OperationID: receipt.OperationID, BindingID: binding.ID, Decision: decision, Principal: cmd.Principal, DecidedAt: now.UTC()}
	_, err = m.commit(ctx, []store.Record{record("operation", receipt.OperationID, op), record("approval_decision", approval.ID, answer)}, nil, []agent.Event{m.event("interaction.responded", in.Scope.TraceID, in.Scope.TurnID, struct {
		InteractionID string `json:"interactionId"`
		Decision      string `json:"decision"`
	}{in.ID, decision})})
	if err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (m *Manager) ClaimApprovedTool(ctx context.Context, frozen agent.FrozenCall, usage agent.Usage, approvalID, executionID string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	claim := ApprovalClaim{ApprovalID: approvalID, CallID: frozen.CallID, ExecutionID: executionID, ClaimedAt: now.UTC()}
	if _, consumed := m.view.ApprovalClaims[approvalID]; consumed {
		return product.NewError(product.CodeStateConflict, "one-time approval is already claimed")
	}
	if err := validateApprovalClaim(m.view, claim); err != nil {
		return err
	}
	call, ok := m.view.Calls[frozen.CallID]
	if !ok || call.Call != frozen || call.Claimed || call.Observation != nil {
		return product.NewError(product.CodeStateConflict, "tool call cannot be claimed")
	}
	tr := m.view.Traces[call.Scope.TraceID]
	if tr == nil || tr.State != "running" {
		return product.NewError(product.CodeStateConflict, "tool trace is not running")
	}
	expected := tr.Usage
	expected.ToolExecutions++
	if usage != expected {
		return product.NewError(product.CodeStateConflict, "tool budget candidate is stale")
	}
	if usage.ToolExecutions > tr.Limits.TraceToolCalls {
		return product.NewError(product.CodeBudgetExhausted, "tool budget exhausted")
	}
	next := *tr
	next.Usage = usage
	call.Claimed = true
	_, err := m.commit(ctx, []store.Record{record("trace", tr.ID, next), record("tool_call", frozen.CallID, call), record("approval_claim", approvalID, claim)}, nil, []agent.Event{m.event("tool.state_changed", tr.ID, call.Scope.TurnID, call)})
	return err
}
