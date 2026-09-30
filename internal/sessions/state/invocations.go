package state

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// Invocation is the durable record of one delegated child agent run. It is a
// control fact, never history: child messages stay out of the parent branch.
type Invocation struct {
	ID                 string            `json:"id"`
	ParentInvocationID string            `json:"parentInvocationId"`
	ParentCallID       string            `json:"parentCallId"`
	TraceID            string            `json:"traceId"`
	Target             agent.TargetAgent `json:"target"`
	State              string            `json:"state"` // running | interrupted | completed | failed | cancelled
	Result             string            `json:"result,omitempty"`
	ModelCalls         int               `json:"modelCalls"`
	// CallIDs are the product tool calls accepted from this child's own model
	// responses, in acceptance order. They are the only calls the child scope
	// may execute; workflow node calls are bound by their node records instead.
	CallIDs []string `json:"callIds,omitempty"`
	// Recovery binds a child restart to the original private history and the
	// host's explicitly versioned build/model/environment. Empty legacy values
	// never authorize a restart.
	RecoveryFingerprint string `json:"recoveryFingerprint,omitempty"`
	RecoveryLeafID      string `json:"recoveryLeafId,omitempty"`
	RecoveryInputID     string `json:"recoveryInputId,omitempty"`
}

func invocationTerminal(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled"
}

// invocationTransition lists the allowed state changes. interrupted is not
// terminal: a stopped process or closed parent leaves the child there.
func invocationTransition(from, to string) bool {
	switch from {
	case "running":
		return to == "interrupted" || invocationTerminal(to)
	case "interrupted":
		return to == "running" || invocationTerminal(to)
	}
	return false
}

// SaveInvocation commits a new running invocation or one allowed transition.
// A new record requires the claimed parent task call. Accepted child calls
// are owned by RegisterChildCalls and are kept unchanged here.
func (m *Manager) SaveInvocation(ctx context.Context, inv Invocation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inv.ID == "" || inv.TraceID == "" || inv.ParentCallID == "" || inv.ParentInvocationID == "" || inv.Target.Name == "" || inv.ModelCalls < 0 {
		return product.NewError(product.CodeInvalidArgument, "invocation identity is incomplete")
	}
	if old, ok := m.view.Invocations[inv.ID]; ok {
		if !invocationTransition(old.State, inv.State) || old.ParentInvocationID != inv.ParentInvocationID || old.ParentCallID != inv.ParentCallID || old.TraceID != inv.TraceID || old.Target != inv.Target || old.RecoveryFingerprint != inv.RecoveryFingerprint || old.RecoveryLeafID != inv.RecoveryLeafID || old.RecoveryInputID != inv.RecoveryInputID || inv.ModelCalls < old.ModelCalls {
			return product.NewError(product.CodeStateConflict, "invocation transition is invalid")
		}
		if inv.State == "running" {
			if tr := m.view.Traces[inv.TraceID]; tr == nil || tr.State != "running" {
				return product.NewError(product.CodeStateConflict, "invocation requires a running trace")
			}
		}
		inv.CallIDs = append([]string(nil), old.CallIDs...)
	} else {
		call, found := m.view.Calls[inv.ParentCallID]
		tr := m.view.Traces[inv.TraceID]
		if inv.State != "running" || len(inv.CallIDs) != 0 || !found || !call.Claimed || call.Observation != nil || call.Scope.TraceID != inv.TraceID || call.Scope.InvocationID != inv.ParentInvocationID || tr == nil || tr.State != "running" {
			return product.NewError(product.CodeStateConflict, "invocation requires a claimed running parent call")
		}
	}
	_, err := m.commit(ctx, []store.Record{record("invocation", inv.ID, inv)}, nil, nil)
	return err
}

// RegisterChildCalls accepts the tool calls of one complete child model
// response. The calls and the invocation's call list are committed together;
// no message, Turn or parent history entry is written. The child has no Turn,
// so each call carries its provider identity and an empty TurnID.
func (m *Manager) RegisterChildCalls(ctx context.Context, invocationID string, calls []agent.ToolRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.view.Invocations[invocationID]
	if !ok || inv.State != "running" {
		return product.NewError(product.CodeStateConflict, "child calls require a running invocation")
	}
	if tr := m.view.Traces[inv.TraceID]; tr == nil || tr.State != "running" {
		return product.NewError(product.CodeStateConflict, "child calls require a running trace")
	}
	if len(calls) == 0 {
		return nil
	}
	controls := make([]store.Record, 0, len(calls)+1)
	events := make([]agent.Event, 0, len(calls))
	seen := map[string]bool{}
	for _, call := range calls {
		c, s := call.Call, call.Scope
		if _, exists := m.view.Calls[c.CallID]; exists || seen[c.CallID] || c.CallID == "" || c.ProviderCallID == "" || c.OperationID != "" || c.Name == "" || c.SelectionRevision != 0 || call.Claimed || call.Observation != nil ||
			s.TraceID != inv.TraceID || s.InvocationID != inv.ID || s.ParentInvocationID != inv.ParentInvocationID || s.TurnID != "" || s.SelectionRevision != 0 {
			return product.NewError(product.CodeStateConflict, "child tool call cannot be registered")
		}
		seen[c.CallID] = true
		inv.CallIDs = append(inv.CallIDs, c.CallID)
		controls = append(controls, record("tool_call", c.CallID, call))
		events = append(events, m.event("tool.requested", inv.TraceID, "", call))
	}
	controls = append(controls, record("invocation", inv.ID, inv))
	_, err := m.commit(ctx, controls, nil, events)
	return err
}

// InvocationForCall returns the child invocation that accepted callID from
// its own model response. Parent, direct and workflow node calls have none.
func (v View) InvocationForCall(callID string) (Invocation, bool) {
	for _, inv := range v.Invocations {
		for _, id := range inv.CallIDs {
			if id == callID {
				return inv, true
			}
		}
	}
	return Invocation{}, false
}

// Invocation returns one invocation without cloning the whole view.
func (m *Manager) Invocation(id string) (Invocation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.view.Invocations[id]
	inv.CallIDs = append([]string(nil), inv.CallIDs...)
	return inv, ok
}

// CompleteChildResume atomically saves the child terminal record, original
// delegate_task observation, parent tool-result message and ended parent Turn.
func (m *Manager) CompleteChildResume(ctx context.Context, invocationID, executionID string, result string, calls int, failedCode string, truncated bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.view.Invocations[invocationID]
	tr := m.view.Traces[inv.TraceID]
	parent, parentOK := m.view.Calls[inv.ParentCallID]
	var segment ResumedExecution
	for _, candidate := range m.view.ResumedExecutions {
		if candidate.ID == executionID && candidate.InvocationID == invocationID {
			segment = candidate
			break
		}
	}
	turn := m.view.Turns[parent.Scope.TurnID]
	if !ok || tr == nil || !parentOK || segment.ID == "" || inv.State != "running" || tr.State != "running" || tr.ExecutionID != executionID || tr.ExecutionStopped || parent.Observation != nil || !parent.Claimed || turn.ID == "" || turn.Ended || !containsID(turn.CallIDs, parent.Call.CallID) || calls < 0 {
		return product.NewError(product.CodeStateConflict, "child resume completion is stale")
	}
	if m.view.invocationUnknown(inv.ID) && failedCode != product.CodeReconciliationRequired {
		return product.NewError(product.CodeReconciliationRequired, "child tool effects require reconciliation")
	}
	status := "completed"
	out := struct {
		Status       string `json:"status"`
		Code         string `json:"code,omitempty"`
		Agent        string `json:"agent,omitempty"`
		InvocationID string `json:"invocationId,omitempty"`
		Result       string `json:"result,omitempty"`
		Truncated    bool   `json:"truncated,omitempty"`
	}{Status: "completed", Agent: inv.Target.Name, InvocationID: inv.ID, Result: result, Truncated: truncated}
	observation := agent.ToolObservation{Status: "succeeded", SideEffect: "none", Executed: true}
	if failedCode != "" {
		status, out.Status, out.Code = "failed", "failed", failedCode
		out.Result, out.Truncated = "", false
		observation.Status = "failed"
		if failedCode == product.CodeReconciliationRequired {
			observation.Status, observation.SideEffect = "outcome_unknown", "unknown"
		}
	}
	raw, _ := json.Marshal(out)
	observation.Content = string(raw)
	inv.State, inv.ModelCalls, inv.Result = status, inv.ModelCalls+calls, out.Result
	parent.Observation = &observation
	entries, events, err := m.toolResultRecords(turn, &parent)
	if err != nil {
		return err
	}
	turn.Ended = true
	controls := []store.Record{record("invocation", inv.ID, inv), record("tool_call", parent.Call.CallID, parent), record("turn", turn.ID, turn)}
	events = append([]agent.Event{m.event("tool.finished", tr.ID, turn.ID, parent)}, events...)
	_, err = m.commit(ctx, controls, entries, events)
	return err
}

// invocationUnknown follows only durable child membership, never executing it.
func (v View) invocationMembers(rootID string) map[string]bool {
	members := map[string]bool{rootID: true}
	for changed := true; changed; {
		changed = false
		for _, inv := range v.Invocations {
			if members[inv.ParentInvocationID] && !members[inv.ID] {
				members[inv.ID], changed = true, true
			}
		}
	}
	return members
}

func (v View) invocationUnknown(rootID string) bool {
	members := v.invocationMembers(rootID)
	for _, call := range v.Calls {
		if members[call.Scope.InvocationID] && v.unresolvedChildCall(call) {
			return true
		}
	}
	return false
}

// validateChildCompletionCommit keeps the child terminal record, immutable
// original parent observation and full original tool group atomic on replay.
func validateChildCompletionCommit(v *View, c store.Commit) error {
	for _, segment := range v.ResumedExecutions {
		inv := v.Invocations[segment.InvocationID]
		tr := v.Traces[inv.TraceID]
		if segment.InvocationID == "" || inv.State != "running" || tr == nil || tr.ExecutionID != segment.ID {
			continue
		}
		parent := v.Calls[inv.ParentCallID]
		turn := v.Turns[parent.Scope.TurnID]
		var nextInvocation Invocation
		var nextParent agent.ToolRecord
		var nextTurn agent.TurnRecord
		invFound, parentFound, turnFound, completing := false, false, false, false
		for _, r := range c.ControlRecords {
			switch {
			case r.Type == "invocation" && r.ID == inv.ID:
				if json.Unmarshal(r.Payload, &nextInvocation) != nil {
					return product.NewError(product.CodeIncompatibleVersion, "invalid child completion")
				}
				invFound = true
				completing = completing || invocationTerminal(nextInvocation.State)
			case r.Type == "tool_call" && r.ID == parent.Call.CallID:
				if json.Unmarshal(r.Payload, &nextParent) != nil {
					return product.NewError(product.CodeIncompatibleVersion, "invalid parent completion")
				}
				parentFound = true
				completing = completing || nextParent.Observation != nil
			case r.Type == "turn" && r.ID == turn.ID:
				if json.Unmarshal(r.Payload, &nextTurn) != nil {
					return product.NewError(product.CodeIncompatibleVersion, "invalid completion turn")
				}
				turnFound = true
				completing = completing || nextTurn.Ended
			}
		}
		for _, entry := range c.Entries {
			var msg agent.AgentMessage
			if json.Unmarshal(entry.Payload, &msg) == nil && msg.Kind == agent.KindToolResult && msg.Scope.TurnID == turn.ID {
				completing = true
			}
		}
		if !completing {
			continue
		}
		if !invFound || !parentFound || !turnFound || len(c.ControlRecords) != 3 || tr.State != "running" || tr.ExecutionStopped || parent.Observation != nil || !parent.Claimed || nextParent.Observation == nil || nextParent.Scope != parent.Scope || nextParent.Call != parent.Call || !nextParent.Claimed || nextInvocation.State != "completed" && nextInvocation.State != "failed" || nextInvocation.ModelCalls < inv.ModelCalls {
			return product.NewError(product.CodeIncompatibleVersion, "partial child completion commit")
		}
		expectedInvocation := inv
		expectedInvocation.State, expectedInvocation.ModelCalls, expectedInvocation.Result = nextInvocation.State, nextInvocation.ModelCalls, nextInvocation.Result
		expectedTurn := turn
		expectedTurn.Ended = true
		if !reflect.DeepEqual(nextInvocation, expectedInvocation) || !reflect.DeepEqual(nextTurn, expectedTurn) || v.invocationUnknown(inv.ID) && !unresolvedObservation(*nextParent.Observation) {
			return product.NewError(product.CodeIncompatibleVersion, "child completion identity or effects differ")
		}
		builder := Manager{sessionID: parent.Scope.SessionID, view: v}
		expectedEntries, _, err := builder.toolResultRecords(turn, &nextParent)
		if err != nil || len(expectedEntries) != len(c.Entries) {
			return product.NewError(product.CodeIncompatibleVersion, "child completion tool group differs")
		}
		for i, entry := range c.Entries {
			var expected, actual agent.AgentMessage
			if json.Unmarshal(expectedEntries[i].Payload, &expected) != nil || json.Unmarshal(entry.Payload, &actual) != nil {
				return product.NewError(product.CodeIncompatibleVersion, "invalid child tool result")
			}
			expected.ID = actual.ID
			if !reflect.DeepEqual(expected, actual) {
				return product.NewError(product.CodeIncompatibleVersion, "child completion tool result differs")
			}
		}
	}
	return nil
}

// CancelInterruptedChild closes a safe delegated call without replaying it.
// The invocation and parent observation are committed together, so a retry
// cannot turn a known no-effect cancellation into an unknown effect.
func (m *Manager) CancelInterruptedChild(ctx context.Context, invocationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.view.Invocations[invocationID]
	tr := m.view.Traces[inv.TraceID]
	parent, found := m.view.Calls[inv.ParentCallID]
	if !ok || !found || tr == nil || tr.State != "cancelling" || !tr.ExecutionStopped || inv.State != "interrupted" || parent.Observation != nil || parent.Call.Name != "delegate_task" || parent.Scope.InvocationID != tr.InvocationID || inv.ParentInvocationID != tr.InvocationID || parent.Scope.TraceID != tr.ID {
		return product.NewError(product.CodeStateConflict, "child cancellation is stale")
	}
	members := m.view.invocationMembers(inv.ID)
	unknown := m.view.invocationUnknown(inv.ID)
	var controls []store.Record
	executed := false
	for _, call := range m.view.Calls {
		if !members[call.Scope.InvocationID] {
			continue
		}
		executed = executed || call.Claimed
		if call.Observation == nil {
			call.Observation = &agent.ToolObservation{Status: "skipped", Content: "cancelled before tool claim", SideEffect: "none"}
			if call.Claimed {
				call.Observation = &agent.ToolObservation{Status: "outcome_unknown", Content: "cancelled without a durable tool result", SideEffect: "unknown", Executed: true}
			}
			controls = append(controls, record("tool_call", call.Call.CallID, call))
		}
	}
	for id := range members {
		child := m.view.Invocations[id]
		if !invocationTerminal(child.State) {
			child.State = "cancelled"
			controls = append(controls, record("invocation", child.ID, child))
		}
	}
	parent.Observation = &agent.ToolObservation{Status: "cancelled", Content: "delegated execution cancelled", SideEffect: "none", Executed: executed}
	if unknown {
		parent.Observation.Status, parent.Observation.SideEffect = "outcome_unknown", "unknown"
	}
	controls = append(controls, record("tool_call", parent.Call.CallID, parent))
	_, err := m.commit(ctx, controls, nil, []agent.Event{m.event("tool.finished", tr.ID, parent.Scope.TurnID, parent)})
	return err
}

func applyInvocation(v *View, r store.Record) error {
	var inv Invocation
	if err := json.Unmarshal(r.Payload, &inv); err != nil {
		return err
	}
	if inv.ID == "" || inv.ID != r.ID {
		return product.NewError(product.CodeIncompatibleVersion, "invocation identity mismatch")
	}
	if old, ok := v.Invocations[inv.ID]; ok {
		if old.ParentInvocationID != inv.ParentInvocationID || old.ParentCallID != inv.ParentCallID || old.TraceID != inv.TraceID || old.Target != inv.Target || old.RecoveryFingerprint != inv.RecoveryFingerprint || old.RecoveryLeafID != inv.RecoveryLeafID || old.RecoveryInputID != inv.RecoveryInputID || inv.ModelCalls < old.ModelCalls {
			return product.NewError(product.CodeIncompatibleVersion, "invocation recovery identity is immutable")
		}
		if invocationTerminal(old.State) {
			return product.NewError(product.CodeStateConflict, "terminal invocation is immutable")
		}
		if old.State != inv.State && !invocationTransition(old.State, inv.State) {
			return product.NewError(product.CodeStateConflict, "invocation transition is invalid")
		}
		if len(inv.CallIDs) < len(old.CallIDs) {
			return product.NewError(product.CodeStateConflict, "accepted child calls are immutable")
		}
		for i, id := range old.CallIDs {
			if inv.CallIDs[i] != id {
				return product.NewError(product.CodeStateConflict, "accepted child calls are immutable")
			}
		}
	}
	if v.Invocations == nil {
		v.Invocations = map[string]Invocation{}
	}
	v.Invocations[inv.ID] = inv
	return nil
}
