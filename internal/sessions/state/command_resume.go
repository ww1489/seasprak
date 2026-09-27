package state

import (
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// DirectResumeBinding is a stopped, unclaimed command, not an Eino checkpoint.
// It lives in the same journal as the original call and approval.
type DirectResumeBinding struct {
	ID                     string               `json:"id"`
	Scope                  agent.ExecutionScope `json:"scope"`
	InputID                string               `json:"inputId"`
	OperationID            string               `json:"operationId"`
	CallID                 string               `json:"callId"`
	InteractionID          string               `json:"interactionId"`
	ApprovalID             string               `json:"approvalId"`
	FrozenHash             string               `json:"frozenHash"`
	BuildCompatibility     string               `json:"buildCompatibility"`
	EnvironmentFingerprint string               `json:"environmentFingerprint"`
	ManifestHash           string               `json:"manifestHash"`
	HistoryCommit          uint64               `json:"historyCommit"`
	LeafID                 string               `json:"leafId"`
}

// ValidateDirectBinding verifies original identities without assuming a current
// execution state; it is also used while replaying an already consumed binding.
func (v View) ValidateDirectBinding(b DirectResumeBinding) error {
	tr := v.Traces[b.Scope.TraceID]
	in := v.Inputs[b.InputID]
	op, exists := v.Operations[b.OperationID]
	approval := v.Approvals[b.ApprovalID]
	interaction := v.Interactions[b.InteractionID]
	frozen, call, err := approvalDescription(&v, approval)
	var envelope struct {
		OperationID, Name string
		Arguments         json.RawMessage
	}
	if b.ID == "" || b.BuildCompatibility == "" || b.EnvironmentFingerprint == "" || b.ManifestHash == "" || b.HistoryCommit == 0 || tr == nil || in == nil || !exists || err != nil || tr.Kind != "command" || tr.Target.Name != "direct-command" || tr.Target.Version != "command-v1" || tr.Generation != b.Scope.Generation || tr.InvocationID != b.Scope.InvocationID || b.Scope.TurnID != "" || b.Scope.ExecutionID == "" || in.Kind != "command" || in.TraceID != tr.ID || in.Target != tr.Target || json.Unmarshal(in.Content, &envelope) != nil || envelope.OperationID != b.OperationID || envelope.Name != op.Receipt.Target || string(envelope.Arguments) != call.Call.Arguments || op.Kind != "direct_command" || op.SessionID != b.Scope.SessionID || b.CallID != b.OperationID || call.Call.CallID != b.CallID || call.Call.OperationID != b.OperationID || call.Scope != b.Scope || frozen.Origin != "direct" || frozen.OperationID != b.OperationID || frozen.Hash != b.FrozenHash || approval.ID != b.ApprovalID || approval.InteractionID != b.InteractionID || interaction.ApprovalID != approval.ID || interaction.CallID != b.CallID || interaction.Scope != b.Scope {
		return product.NewError(product.CodeIncompatibleResume, "direct resume binding differs from the original command")
	}
	return nil
}

func applyDirectResume(v *View, r store.Record) error {
	var b DirectResumeBinding
	if json.Unmarshal(r.Payload, &b) != nil || b.HistoryCommit != v.LastSeq || b.LeafID != v.LeafID || v.ValidateDirectBinding(b) != nil {
		return product.NewError(product.CodeIncompatibleVersion, "invalid direct resume binding")
	}
	call := v.Calls[b.CallID]
	tr := v.Traces[b.Scope.TraceID]
	if tr.State != "running" || !tr.Started || tr.Settled || call.Claimed || call.Observation != nil || v.Inputs[b.InputID].State != "pending" || v.Operations[b.OperationID].State != "running" {
		return product.NewError(product.CodeIncompatibleVersion, "direct resume binding has no pending command")
	}
	return putImmutable(&v.DirectResumes, r.ID, b.ID, b)
}

// CommitCommandWait is called by the coordinator only after the worker exits.
func (m *Manager) CommitCommandWait(ctx context.Context, b DirectResumeBinding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b.Scope.SessionID != m.sessionID || b.Scope.BranchID != m.view.BranchID || b.HistoryCommit != m.view.LastSeq || m.view.HasUnresolvedEffects() || m.view.ValidateDirectBinding(b) != nil {
		return product.NewError(product.CodeStateConflict, "command cannot enter approval wait")
	}
	tr := m.view.Traces[b.Scope.TraceID]
	call := m.view.Calls[b.CallID]
	if tr.State != "running" || !tr.Started || tr.Settled || call.Claimed || call.Observation != nil {
		return product.NewError(product.CodeStateConflict, "command is no longer awaiting approval")
	}
	next := *tr
	next.State, next.ExecutionID, next.DirectResumeID = "paused", b.Scope.ExecutionID, b.ID
	next.ExecutionStopped = true
	binding := ApprovalBinding{ID: b.ID + ":" + b.InteractionID, InteractionID: b.InteractionID, ApprovalID: b.ApprovalID, DirectResumeID: b.ID}
	_, err := m.commit(ctx, []store.Record{record("direct_resume", b.ID, b), record("approval_binding", binding.ID, binding), record("trace", tr.ID, next)}, nil, []agent.Event{m.event("interaction.ready", tr.ID, "", struct {
		InteractionID string `json:"interactionId"`
	}{b.InteractionID}), m.event("trace.state_changed", tr.ID, "", next)})
	return err
}

func (m *Manager) CommitCommandResume(ctx context.Context, cmd OperationCommand, bindingID, executionID string) (OperationReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if receipt, found, err := m.findOperation(cmd); found || err != nil {
		return receipt, err
	}
	b, exists := m.view.DirectResumes[bindingID]
	tr := m.view.Traces[cmd.Target]
	if !exists || tr == nil || cmd.Kind != "resume" || cmd.ExpectedRevision != m.view.LastSeq || tr.ID != b.Scope.TraceID || tr.DirectResumeID != b.ID || tr.ExecutionID != b.Scope.ExecutionID || tr.State != "paused" || !tr.ExecutionStopped || tr.Settled || executionID == "" || executionID == tr.ExecutionID || m.view.HasUnresolvedEffects() || m.view.ValidateDirectBinding(b) != nil || m.view.Calls[b.CallID].Claimed || m.view.Calls[b.CallID].Observation != nil {
		return OperationReceipt{}, product.NewError(product.CodeIncompatibleResume, "direct command is not safely waiting")
	}
	if _, answered := m.view.ApprovalDecisions[b.ApprovalID]; !answered {
		return OperationReceipt{}, product.NewError(product.CodeStateConflict, "direct command approval is unanswered")
	}
	digest, err := operationDigest(cmd)
	if err != nil {
		return OperationReceipt{}, err
	}
	receipt := OperationReceipt{OperationID: agent.MustID(), State: "accepted", Target: tr.ID, AcceptedCommit: m.view.LastSeq + 1}
	op := Operation{Receipt: receipt, Principal: cmd.Principal, SessionID: m.sessionID, Kind: cmd.Kind, Key: cmd.IdempotencyKey, Digest: digest, Revision: 1, State: "accepted"}
	scope := b.Scope
	scope.ExecutionID = executionID
	segment := ResumedExecution{ID: executionID, DirectResumeID: b.ID, OperationID: receipt.OperationID, Scope: scope}
	next := *tr
	next.State, next.ExecutionID, next.DirectResumeID = "running", executionID, ""
	next.ExecutionStopped = false
	_, err = m.commit(ctx, []store.Record{record("operation", receipt.OperationID, op), record("resumed_execution", executionID, segment), record("trace", tr.ID, next)}, nil, []agent.Event{m.event("trace.state_changed", tr.ID, "", next)})
	if err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
