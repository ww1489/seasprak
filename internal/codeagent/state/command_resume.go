package state

import (
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// DirectResumeBinding is a legacy stopped command retained for journal replay.
// New host shell calls never create or execute this recovery representation.
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

// ValidateDirectBinding validates historical identities, never execution rights.
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
