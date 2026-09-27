package state

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
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

// RequestApproval is an asked fact, never an execution permission. Repeating the
// same pending frozen request preserves its IDs and original expiry.
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

// CommitApprovalCheckpoint is called only with actual worker-exit and blob
// evidence. The binding, checkpoint and stopped waiting trace share one commit.
func (m *Manager) CommitApprovalCheckpoint(ctx context.Context, traceID string, cp CheckpointRef, targets map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commitApprovalCheckpoint(ctx, traceID, cp, targets, "")
}

// CommitApprovalPause completes an accepted explicit Pause in the same commit
// as the business interrupt targets, checkpoint and stopped proof.
func (m *Manager) CommitApprovalPause(ctx context.Context, traceID, operationID string, cp CheckpointRef, targets map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commitApprovalCheckpoint(ctx, traceID, cp, targets, operationID)
}

func (m *Manager) commitApprovalCheckpoint(ctx context.Context, traceID string, cp CheckpointRef, targets map[string]string, pauseID string) error {
	tr := m.view.Traces[traceID]
	if tr == nil || tr.State != "running" || !tr.Started || tr.Settled || cp.ID == "" || cp.BlobHash == "" || cp.BlobSize <= 0 || cp.Scope.SessionID != m.sessionID || cp.Scope.BranchID != m.view.BranchID || cp.Scope.TraceID != traceID || cp.Scope.InvocationID != tr.InvocationID || cp.Scope.Generation != tr.Generation || cp.Target != tr.Target || cp.HistoryCommit != m.view.LastSeq || cp.ProjectionRevision != cp.HistoryCommit || cp.LeafID != m.view.LeafID || len(cp.InteractionIDs) == 0 || len(targets) != len(cp.InteractionIDs) || m.view.HasUnresolvedEffects() {
		return product.NewError(product.CodeStateConflict, "approval checkpoint association is invalid")
	}
	next := *tr
	next.State, next.ExecutionID, next.CheckpointID = "paused", cp.Scope.ExecutionID, cp.ID
	next.ExecutionStopped, next.Settled = true, false
	controls := []store.Record{record("checkpoint_ref", cp.ID, cp), record("trace", traceID, next)}
	var events []agent.Event
	seen := make(map[string]bool)
	seenTargets := make(map[string]bool)
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
		// Framework target addresses remain private, including in durable events.
		events = append(events, m.event("interaction.ready", traceID, in.Scope.TurnID, struct {
			InteractionID string `json:"interactionId"`
		}{id}))
	}
	if pauseID != "" {
		op, exists := m.view.Operations[pauseID]
		if !exists || op.Kind != "pause" || op.Receipt.Target != traceID || op.State != "accepted" {
			return product.NewError(product.CodeStateConflict, "approval pause operation is not accepted")
		}
		op.State, op.ResultRef = "completed", cp.ID
		op.Revision++
		controls = append(controls, record("operation", pauseID, op))
	}
	events = append(events, m.event("trace.state_changed", traceID, cp.Scope.TurnID, next))
	_, err := m.commit(ctx, controls, nil, events)
	return err
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

// RespondApproval records a decision and its durable acceptance together. It
// neither claims the operation nor changes the paused trace or checkpoint.
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
	return m.claimTool(ctx, frozen, usage, &claim)
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
