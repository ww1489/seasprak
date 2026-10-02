package state

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

type ResumedExecution struct {
	ID             string `json:"id"`
	CheckpointID   string `json:"checkpointId,omitempty"`
	DirectResumeID string `json:"directResumeId,omitempty"`
	// InvocationID identifies a safe interrupted-child recovery segment. It is
	// mutually exclusive with checkpoint and direct-command recovery.
	InvocationID string               `json:"invocationId,omitempty"`
	ParentCallID string               `json:"parentCallId,omitempty"`
	OperationID  string               `json:"operationId"`
	Scope        agent.ExecutionScope `json:"scope"`
}

func applyResumedExecution(v *View, record store.Record) error {
	var segment ResumedExecution
	if err := json.Unmarshal(record.Payload, &segment); err != nil {
		return err
	}
	cp, ok := v.Checkpoints[segment.CheckpointID]
	if segment.InvocationID != "" {
		inv, exists := v.Invocations[segment.InvocationID]
		tr := v.Traces[inv.TraceID]
		op, accepted := v.Operations[segment.OperationID]
		if !exists || tr == nil {
			return product.NewError(product.CodeIncompatibleVersion, "invalid child resumed execution")
		}
		parent := v.Calls[inv.ParentCallID]
		expected := agent.ExecutionScope{SessionID: parent.Scope.SessionID, BranchID: parent.Scope.BranchID, TraceID: inv.TraceID, InvocationID: tr.InvocationID, ExecutionID: segment.ID, Generation: tr.Generation}
		if segment.CheckpointID != "" || segment.DirectResumeID != "" || segment.ParentCallID == "" || segment.ParentCallID != inv.ParentCallID || !accepted || op.Kind != "resume" || op.SessionID != expected.SessionID || op.Receipt.Target != inv.TraceID || op.Receipt.AcceptedCommit != v.LastSeq+1 || segment.ID == "" || segment.Scope != expected || tr.State != "paused" || !tr.ExecutionStopped || inv.State != "interrupted" {
			return product.NewError(product.CodeIncompatibleVersion, "invalid child resumed execution")
		}
		return putImmutable(&v.ResumedExecutions, record.ID, segment.ID, segment)
	}
	if segment.DirectResumeID != "" {
		b, exists := v.DirectResumes[segment.DirectResumeID]
		tr := v.Traces[b.Scope.TraceID]
		op, accepted := v.Operations[segment.OperationID]
		expected := b.Scope
		expected.ExecutionID = segment.ID
		if !exists || segment.CheckpointID != "" || !accepted || op.Kind != "resume" || op.Receipt.Target != b.Scope.TraceID || op.Receipt.AcceptedCommit != v.LastSeq+1 || segment.ID == b.Scope.ExecutionID || segment.Scope != expected || tr == nil || tr.State != "paused" || !tr.ExecutionStopped || tr.DirectResumeID != b.ID || v.ValidateDirectBinding(b) != nil {
			return product.NewError(product.CodeIncompatibleVersion, "invalid direct resumed execution")
		}
		return putImmutable(&v.ResumedExecutions, record.ID, segment.ID, segment)
	}
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

// InterruptedChildForCall is the narrow exception to ordinary interrupted
// tool finalization. No child or descendant tool that was claimed may replay.
func (v View) InterruptedChildForCall(callID string) (Invocation, bool) {
	call, ok := v.Calls[callID]
	turn := v.Turns[call.Scope.TurnID]
	tr := v.Traces[call.Scope.TraceID]
	if !ok || call.Call.Name != "delegate_task" || !call.Claimed || call.Observation != nil || call.Call.ProviderCallID == "" || call.Scope.TurnID == "" || tr == nil || call.Scope.InvocationID != tr.InvocationID || call.Scope.BranchID != v.BranchID || call.Scope.Generation != tr.Generation || turn.ID == "" || turn.Ended || turn.TraceID != tr.ID || turn.InvocationID != tr.InvocationID || !containsID(turn.CallIDs, callID) || v.ReconciliationUnresolved(callID) {
		return Invocation{}, false
	}
	var root Invocation
	for _, inv := range v.Invocations {
		if inv.TraceID == call.Scope.TraceID && inv.ParentCallID == callID && inv.ParentInvocationID == call.Scope.InvocationID && inv.State == "interrupted" {
			if root.ID != "" {
				return Invocation{}, false
			}
			root = inv
		}
	}
	if root.ID == "" {
		return Invocation{}, false
	}
	for _, inv := range v.Invocations {
		if inv.TraceID == root.TraceID && inv.ParentInvocationID == root.ID {
			return Invocation{}, false
		}
	}
	for _, childCall := range v.Calls {
		if childCall.Scope.TraceID == root.TraceID && childCall.Scope.InvocationID == root.ID && (childCall.Claimed || v.unresolvedChildCall(childCall)) {
			return Invocation{}, false
		}
	}
	return root, true
}

func (v View) unresolvedChildCall(call agent.ToolRecord) bool {
	if v.ReconciliationUnresolved(call.Call.CallID) {
		return true
	}
	if effective, ok := v.EffectiveObservation(call.Call.CallID); ok {
		return unresolvedObservation(effective.Observation)
	}
	return unknownEffect(call) || call.Claimed && call.Observation == nil
}

func (v View) validateChildResumePoint(invocationID string) error {
	inv, ok := v.Invocations[invocationID]
	tr := v.Traces[inv.TraceID]
	if !ok || tr == nil || tr.State != "paused" || !tr.Started || tr.Settled || !tr.ExecutionStopped || v.ActiveTrace != tr.ID || tr.CheckpointID != "" || tr.DirectResumeID != "" || inv.RecoveryFingerprint == "" || inv.RecoveryLeafID != v.LeafID || inv.Target.Generation != tr.Generation {
		return product.NewError(product.CodeIncompatibleResume, "interrupted child is not safely resumable")
	}
	root, safe := v.InterruptedChildForCall(inv.ParentCallID)
	input := v.Inputs[inv.RecoveryInputID]
	if !safe || root.ID != inv.ID || input == nil || input.State != "consumed" || input.TraceID != tr.ID || input.Kind != "prompt" {
		return product.NewError(product.CodeIncompatibleResume, "child original call or consumed input differs")
	}
	for id, call := range v.Calls {
		if id != inv.ParentCallID && v.unresolvedChildCall(call) {
			return product.NewError(product.CodeReconciliationRequired, "other tool effects require reconciliation")
		}
	}
	return nil
}

// validateChildResumeCommit rejects partial acceptance on both write and replay.
// A recovery segment, trace and invocation transition must share one commit.
func validateChildResumeCommit(v *View, c store.Commit) error {
	mapped := map[string]int{}
	for _, r := range c.ControlRecords {
		if r.Type == "resumed_execution" {
			var segment ResumedExecution
			if json.Unmarshal(r.Payload, &segment) == nil && segment.InvocationID != "" {
				mapped[segment.InvocationID]++
			}
		}
	}
	// Require the mapping from the transitions too, so deleting its entire
	// record cannot bypass this validation.
	for _, r := range c.ControlRecords {
		if r.Type == "invocation" {
			var next Invocation
			if json.Unmarshal(r.Payload, &next) == nil && v.Invocations[r.ID].State == "interrupted" && next.State == "running" && mapped[r.ID] != 1 {
				return product.NewError(product.CodeIncompatibleVersion, "child restart has no unique recovery mapping")
			}
		}
		if r.Type == "trace" {
			var next TraceState
			old := v.Traces[r.ID]
			if json.Unmarshal(r.Payload, &next) == nil && old != nil && old.State == "paused" && next.State == "running" && old.CheckpointID == "" && old.DirectResumeID == "" {
				for _, inv := range v.Invocations {
					if inv.TraceID == old.ID && inv.ParentInvocationID == old.InvocationID && inv.State == "interrupted" && v.Calls[inv.ParentCallID].Scope.TurnID != "" && mapped[inv.ID] != 1 {
						return product.NewError(product.CodeIncompatibleVersion, "child resumed trace has no unique recovery mapping")
					}
				}
			}
		}
	}
	for _, r := range c.ControlRecords {
		if r.Type != "resumed_execution" {
			continue
		}
		var segment ResumedExecution
		if err := json.Unmarshal(r.Payload, &segment); err != nil {
			return err
		}
		if segment.InvocationID == "" {
			continue
		}
		if err := v.validateChildResumePoint(segment.InvocationID); err != nil {
			return err
		}
		inv := v.Invocations[segment.InvocationID]
		tr := v.Traces[inv.TraceID]
		parent := v.Calls[inv.ParentCallID]
		expectedScope := agent.ExecutionScope{SessionID: parent.Scope.SessionID, BranchID: v.BranchID, TraceID: tr.ID, InvocationID: tr.InvocationID, ExecutionID: segment.ID, Generation: tr.Generation}
		if r.ID != segment.ID || segment.ID == "" || segment.ID == parent.Scope.ExecutionID || segment.ID == tr.ExecutionID || segment.Scope != expectedScope || segment.ParentCallID != inv.ParentCallID || segment.CheckpointID != "" || segment.DirectResumeID != "" || len(c.Entries) != 0 {
			return product.NewError(product.CodeIncompatibleVersion, "child resumed execution scope differs")
		}
		nextTrace := *tr
		nextTrace.State, nextTrace.ExecutionID, nextTrace.CheckpointID, nextTrace.ExecutionStopped = "running", segment.ID, "", false
		nextInvocation := inv
		nextInvocation.State = "running"
		traceFound, invocationFound, operationFound := false, false, false
		stoppedCalls := map[string]agent.ToolRecord{}
		for id, call := range v.Calls {
			if call.Scope.InvocationID == inv.ID && call.Observation == nil {
				call.Observation = &agent.ToolObservation{Status: "cancelled", Content: "interrupted before tool claim", SideEffect: "none"}
				stoppedCalls[id] = call
			}
		}
		for _, record := range c.ControlRecords {
			switch record.Type {
			case "trace":
				var saved TraceState
				if json.Unmarshal(record.Payload, &saved) != nil || record.ID != tr.ID || traceFound || !reflect.DeepEqual(saved, nextTrace) {
					return product.NewError(product.CodeIncompatibleVersion, "child resumed trace differs")
				}
				traceFound = true
			case "invocation":
				var saved Invocation
				if json.Unmarshal(record.Payload, &saved) != nil || record.ID != inv.ID || invocationFound || !reflect.DeepEqual(saved, nextInvocation) {
					return product.NewError(product.CodeIncompatibleVersion, "child resumed invocation differs")
				}
				invocationFound = true
			case "operation":
				var saved Operation
				if json.Unmarshal(record.Payload, &saved) != nil || record.ID != segment.OperationID || operationFound || saved.SessionID != parent.Scope.SessionID || saved.Kind != "resume" || saved.State != "accepted" || saved.Receipt.Target != tr.ID || saved.Receipt.AcceptedCommit != c.CommitSeq {
					return product.NewError(product.CodeIncompatibleVersion, "child resume acceptance differs")
				}
				operationFound = true
			case "resumed_execution":
				if record.ID != segment.ID {
					return product.NewError(product.CodeIncompatibleVersion, "multiple child recovery segments")
				}
			case "tool_call":
				var call agent.ToolRecord
				expected, exists := stoppedCalls[record.ID]
				if json.Unmarshal(record.Payload, &call) != nil || !exists || !reflect.DeepEqual(call, expected) {
					return product.NewError(product.CodeIncompatibleVersion, "child old call cancellation differs")
				}
				delete(stoppedCalls, record.ID)
			default:
				return product.NewError(product.CodeIncompatibleVersion, "unexpected child recovery record")
			}
		}
		if !traceFound || !invocationFound || !operationFound || len(stoppedCalls) != 0 {
			return product.NewError(product.CodeIncompatibleVersion, "partial child resume commit")
		}
	}
	return nil
}

// CommitChildResume accepts recovery of exactly one interrupted child and the
// stopped parent trace in a single commit. It reuses the original invocation
// and parent call identities. Any claimed call in the child invocation tree,
// recorded child observation, or unresolved reconciliation blocks replay.
func (m *Manager) CommitChildResume(ctx context.Context, cmd OperationCommand, invocationID, executionID string) (OperationReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if receipt, found, err := m.findOperation(cmd); found || err != nil {
		return receipt, err
	}
	if cmd.Kind != "resume" || cmd.ExpectedRevision != m.view.LastSeq {
		return OperationReceipt{}, product.NewError(product.CodeStateConflict, "invalid resume operation or revision")
	}
	if err := m.view.validateChildResumePoint(invocationID); err != nil {
		return OperationReceipt{}, err
	}
	inv := m.view.Invocations[invocationID]
	tr := m.view.Traces[inv.TraceID]
	parent := m.view.Calls[inv.ParentCallID]
	if cmd.Target != inv.TraceID || parent.Scope.SessionID != m.sessionID || executionID == "" || executionID == tr.ExecutionID || executionID == parent.Scope.ExecutionID {
		return OperationReceipt{}, product.NewError(product.CodeIncompatibleResume, "interrupted child recovery scope differs")
	}
	var stoppedCalls []agent.ToolRecord
	for _, call := range m.view.Calls {
		if call.Scope.InvocationID != inv.ID || call.Observation != nil {
			continue
		}
		call.Observation = &agent.ToolObservation{Status: "cancelled", Content: "interrupted before tool claim", SideEffect: "none", Executed: false}
		stoppedCalls = append(stoppedCalls, call)
	}
	receipt := OperationReceipt{OperationID: agent.MustID(), State: "accepted", Target: tr.ID, AcceptedCommit: m.view.LastSeq + 1}
	digest, err := operationDigest(cmd)
	if err != nil {
		return OperationReceipt{}, err
	}
	op := Operation{Receipt: receipt, Principal: cmd.Principal, SessionID: m.sessionID, Kind: "resume", Key: cmd.IdempotencyKey, Digest: digest, Revision: 1, State: "accepted"}
	nextTrace := *tr
	nextTrace.State, nextTrace.ExecutionID, nextTrace.CheckpointID, nextTrace.ExecutionStopped = "running", executionID, "", false
	nextInvocation := inv
	nextInvocation.State = "running"
	scope := agent.ExecutionScope{SessionID: parent.Scope.SessionID, BranchID: parent.Scope.BranchID, TraceID: tr.ID, InvocationID: tr.InvocationID, ExecutionID: executionID, Generation: tr.Generation}
	segment := ResumedExecution{ID: executionID, InvocationID: inv.ID, ParentCallID: inv.ParentCallID, OperationID: receipt.OperationID, Scope: scope}
	controls := []store.Record{record("operation", receipt.OperationID, op), record("resumed_execution", executionID, segment), record("trace", tr.ID, nextTrace), record("invocation", inv.ID, nextInvocation)}
	for _, call := range stoppedCalls {
		controls = append(controls, record("tool_call", call.Call.CallID, call))
	}
	_, err = m.commit(ctx, controls, nil, []agent.Event{m.event("trace.state_changed", tr.ID, parent.Scope.TurnID, nextTrace)})
	if err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
