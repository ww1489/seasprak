package state

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// ApprovalBinding names either a server-owned Eino interrupt target or an
// original direct command wait. The two recovery representations are disjoint.
type ApprovalBinding struct {
	ID             string `json:"id"`
	InteractionID  string `json:"interactionId"`
	ApprovalID     string `json:"approvalId"`
	CheckpointID   string `json:"checkpointId,omitempty"`
	DirectResumeID string `json:"directResumeId,omitempty"`
	TargetRef      string `json:"targetRef"`
}

type ApprovalDecision struct {
	ApprovalID    string    `json:"approvalId"`
	InteractionID string    `json:"interactionId"`
	OperationID   string    `json:"operationId"`
	BindingID     string    `json:"bindingId"`
	Decision      string    `json:"decision"`
	Principal     string    `json:"principal"`
	DecidedAt     time.Time `json:"decidedAt"`
}

type ApprovalClaim struct {
	ApprovalID  string    `json:"approvalId"`
	CallID      string    `json:"callId"`
	ExecutionID string    `json:"executionId"`
	ClaimedAt   time.Time `json:"claimedAt"`
}

func validApprovalBinding(v *View, binding ApprovalBinding) bool {
	in := v.Interactions[binding.InteractionID]
	approval := v.Approvals[binding.ApprovalID]
	if in.Kind != "approval" || in.ApprovalID != approval.ID || approval.InteractionID != in.ID {
		return false
	}
	if binding.DirectResumeID != "" {
		b, exists := v.DirectResumes[binding.DirectResumeID]
		return exists && binding.CheckpointID == "" && binding.TargetRef == "" && binding.ID == b.ID+":"+in.ID && b.InteractionID == in.ID && b.ApprovalID == approval.ID && v.ValidateDirectBinding(b) == nil
	}
	cp, exists := v.Checkpoints[binding.CheckpointID]
	return exists && binding.ID == cp.ID+":"+in.ID && binding.TargetRef != "" && containsID(cp.InteractionIDs, in.ID) && containsID(cp.CallIDs, approval.CallID) && approvalCheckpointCall(v, cp, v.Calls[approval.CallID])
}

func validApprovalDecision(decision string) bool {
	return decision == "allowed-once" || decision == "rejected" || decision == "cancelled"
}

func approvalPolicy(v *View, frozen FrozenExecution) error {
	p := v.ExecutionPolicy
	if _, err := NormalizeExecutionPolicy(p); err != nil {
		return err
	}
	if p.Ref == "" || frozen.PolicyRef != p.Ref || p.ApprovalPolicy == "never" || (p.SandboxMode == "read-only" && frozen.Effect != "read" && frozen.Effect != "none") {
		return product.NewError(product.CodePermissionDenied, "current execution policy denies approval")
	}
	return nil
}

func approvalDescription(v *View, approval Approval) (FrozenExecution, agent.ToolRecord, error) {
	frozen, exists := v.FrozenExecutions[approval.FrozenExecutionID]
	call, accepted := v.Calls[approval.CallID]
	digest, err := frozen.Digest()
	if !exists || !accepted || err != nil || frozen.Hash == "" || digest != frozen.Hash || frozen.Hash != approval.FrozenHash || frozen.Scope != approval.Scope || call.Scope != approval.Scope || frozen.CallID != approval.CallID || frozen.ID != "execution:"+approval.CallID || frozen.RequestedGrantRef == "" || frozen.RequestedGrantRef != approval.GrantRef || frozen.Tool != call.Call.Name || frozen.ProviderCallID != call.Call.ProviderCallID || frozen.Generation != call.Call.Generation || frozen.Generation != call.Scope.Generation {
		return FrozenExecution{}, agent.ToolRecord{}, product.NewError(product.CodePermissionDenied, "approval does not match the original frozen call")
	}
	return frozen, call, nil
}

func approvalTime(approval Approval, now time.Time) error {
	if now.IsZero() || approval.ExpiresAt.IsZero() || !now.Before(approval.ExpiresAt) || now.Before(approval.ExpiresAt.Add(-config.ApprovalValidity)) {
		return product.NewError(product.CodePermissionDenied, "approval is expired or outside its validity interval")
	}
	return nil
}

func containsID(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func approvalCheckpointCall(v *View, cp CheckpointRef, call agent.ToolRecord) bool {
	scope := cp.Scope
	seen := make(map[string]bool)
	for scope != call.Scope {
		segment, exists := v.ResumedExecutions[scope.ExecutionID]
		original, saved := v.Checkpoints[segment.CheckpointID]
		expected := original.Scope
		expected.ExecutionID = scope.ExecutionID
		if !exists || !saved || seen[scope.ExecutionID] || segment.Scope != scope || expected != scope || !containsID(original.CallIDs, call.Call.CallID) {
			return false
		}
		seen[scope.ExecutionID] = true
		scope = original.Scope
	}
	return true
}

func responseContent(cmd OperationCommand, decision string) error {
	var body struct {
		Decision string `json:"decision"`
	}
	decoder := json.NewDecoder(bytes.NewReader(cmd.Content))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || body.Decision != decision || !validApprovalDecision(decision) {
		return product.NewError(product.CodeInvalidArgument, "approval response must contain only one permitted decision")
	}
	if err := decoder.Decode(&body); err != io.EOF {
		return product.NewError(product.CodeInvalidArgument, "approval response has trailing content")
	}
	return nil
}

func validateApprovalClaim(v *View, claim ApprovalClaim) error {
	approval, exists := v.Approvals[claim.ApprovalID]
	decision, answered := v.ApprovalDecisions[claim.ApprovalID]
	binding := v.ApprovalBindings[decision.BindingID]
	segment, resumed := v.ResumedExecutions[claim.ExecutionID]
	tr := v.Traces[approval.Scope.TraceID]
	if !exists || !answered || !resumed || claim.CallID != approval.CallID || decision.Decision != "allowed-once" || decision.Principal == "" || decision.ApprovalID != approval.ID || binding.ApprovalID != approval.ID || !validApprovalBinding(v, binding) || binding.CheckpointID != segment.CheckpointID || binding.DirectResumeID != segment.DirectResumeID || tr == nil || tr.State != "running" || tr.Settled || tr.ExecutionStopped || tr.ExecutionID != claim.ExecutionID {
		return product.NewError(product.CodePermissionDenied, "execution has no matching one-time approval")
	}
	if err := approvalTime(approval, claim.ClaimedAt); err != nil {
		return err
	}
	if claim.ClaimedAt.Before(decision.DecidedAt) {
		return product.NewError(product.CodePermissionDenied, "claim precedes the approval decision")
	}
	frozen, call, err := approvalDescription(v, approval)
	if err != nil {
		return err
	}
	cp := v.Checkpoints[segment.CheckpointID]
	original := containsID(cp.CallIDs, claim.CallID) && approvalCheckpointCall(v, cp, call)
	if segment.DirectResumeID != "" {
		b, exists := v.DirectResumes[segment.DirectResumeID]
		original = exists && b.CallID == claim.CallID && b.Scope == call.Scope && v.ValidateDirectBinding(b) == nil
	}
	if call.Observation != nil || !original {
		return product.NewError(product.CodePermissionDenied, "approval is not for a resumable original call")
	}
	return approvalPolicy(v, frozen)
}

// validateApprovalCommit checks the pre-commit view as well as the entire batch.
// Replaying separate claim, intent and budget facts cannot reconstruct an atomic
// allowance consumption that was never durably recorded together.
func validateApprovalCommit(v *View, c store.Commit) error {
	var claim *ApprovalClaim
	for _, r := range c.ControlRecords {
		if r.Type != "approval_claim" {
			continue
		}
		if claim != nil {
			return product.NewError(product.CodeIncompatibleVersion, "approval claim commit contains multiple claims")
		}
		claim = &ApprovalClaim{}
		if json.Unmarshal(r.Payload, claim) != nil || r.ID != claim.ApprovalID {
			return product.NewError(product.CodeIncompatibleVersion, "invalid approval claim identity")
		}
	}
	if claim == nil {
		return nil
	}
	call, exists := v.Calls[claim.CallID]
	tr := v.Traces[call.Scope.TraceID]
	_, consumed := v.ApprovalClaims[claim.ApprovalID]
	if !exists || call.Claimed || call.Observation != nil || tr == nil || consumed || validateApprovalClaim(v, *claim) != nil {
		return product.NewError(product.CodeIncompatibleVersion, "approval claim does not consume a pending call")
	}
	expectedCall := call
	expectedCall.Claimed = true
	expectedTrace := *tr
	expectedTrace.Usage.ToolExecutions++
	traceCount, callCount := 0, 0
	for _, r := range c.ControlRecords {
		if r.Type == "tool_call" && r.ID == claim.CallID {
			var next agent.ToolRecord
			if json.Unmarshal(r.Payload, &next) != nil || !reflect.DeepEqual(next, expectedCall) {
				return product.NewError(product.CodeIncompatibleVersion, "approval claim has no matching original call intent")
			}
			callCount++
		}
		if r.Type == "trace" && r.ID == tr.ID {
			var next TraceState
			if json.Unmarshal(r.Payload, &next) != nil || !reflect.DeepEqual(next, expectedTrace) {
				return product.NewError(product.CodeIncompatibleVersion, "approval claim has no matching budget reservation")
			}
			traceCount++
		}
	}
	if traceCount != 1 || callCount != 1 {
		return product.NewError(product.CodeIncompatibleVersion, "approval claim requires intent and budget in the same commit")
	}
	return nil
}

func applyApprovalRecord(v *View, r store.Record) error {
	switch r.Type {
	case "approval_binding":
		var binding ApprovalBinding
		if err := json.Unmarshal(r.Payload, &binding); err != nil {
			return err
		}
		if !validApprovalBinding(v, binding) {
			return product.NewError(product.CodeIncompatibleVersion, "invalid approval checkpoint binding")
		}
		return putImmutable(&v.ApprovalBindings, r.ID, binding.ID, binding)
	case "approval_decision":
		var answer ApprovalDecision
		if err := json.Unmarshal(r.Payload, &answer); err != nil {
			return err
		}
		approval := v.Approvals[answer.ApprovalID]
		binding := v.ApprovalBindings[answer.BindingID]
		op := v.Operations[answer.OperationID]
		frozen, call, err := approvalDescription(v, approval)
		if err != nil || call.Claimed || call.Observation != nil || approval.State != "asked" || !validApprovalDecision(answer.Decision) || answer.Principal == "" || answer.Principal != op.Principal || op.Kind != "respond_interaction" || op.SessionID != approval.Scope.SessionID || op.Receipt.Target != answer.InteractionID || op.Receipt.AcceptedCommit != v.LastSeq+1 || answer.InteractionID != approval.InteractionID || binding.ApprovalID != approval.ID || binding.InteractionID != answer.InteractionID || !validApprovalBinding(v, binding) || approvalTime(approval, answer.DecidedAt) != nil || approvalPolicy(v, frozen) != nil {
			return product.NewError(product.CodeIncompatibleVersion, "invalid approval decision")
		}
		return putImmutable(&v.ApprovalDecisions, r.ID, answer.ApprovalID, answer)
	case "approval_claim":
		var claim ApprovalClaim
		if err := json.Unmarshal(r.Payload, &claim); err != nil {
			return err
		}
		if !v.Calls[claim.CallID].Claimed || validateApprovalClaim(v, claim) != nil {
			return product.NewError(product.CodeIncompatibleVersion, "invalid one-time approval claim")
		}
		return putImmutable(&v.ApprovalClaims, r.ID, claim.ApprovalID, claim)
	default:
		return product.NewError(product.CodeIncompatibleVersion, "unknown approval record")
	}
}
