package state

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// childAttemptRecords prepares only private control facts. It never creates a
// parent Turn or branch entry, and leaves the started attempt unchanged.
func childAttemptRecords(v *View, initial ModelAttempt, status string, msg *agent.AgentMessage, calls []agent.ToolRecord) ([]store.Record, error) {
	if initial.Purpose == "workflow_node" {
		return nil, product.NewError(product.CodeIncompatibleVersion, "embedded workflow model attempts are no longer supported")
	}
	scope := initial.Scope
	inv, ok := v.Invocations[scope.InvocationID]
	tr := v.Traces[scope.TraceID]
	parent, found := v.Calls[inv.ParentCallID]
	// Cancellation closes existing attempts before execution exit; it never
	// admits a complete response or new work after the cancellation intent.
	closing := status == "failed" || status == "incomplete" || status == "aborted"
	if !ok || inv.State != "running" || inv.TraceID != scope.TraceID || inv.ParentInvocationID != scope.ParentInvocationID || scope.ParentInvocationID == "" || scope.TurnID != "" || scope.SelectionRevision != 0 ||
		tr == nil || tr.State != "running" && (tr.State != "cancelling" || !closing) || tr.ExecutionStopped || !found || !parent.Claimed || parent.Observation != nil || parent.Scope.SessionID != scope.SessionID || parent.Scope.TraceID != inv.TraceID || parent.Scope.InvocationID != inv.ParentInvocationID {
		return nil, product.NewError(product.CodeStateConflict, "child attempt requires its running invocation and parent")
	}
	switch status {
	case "accepted", "failed", "incomplete", "aborted":
	default:
		return nil, product.NewError(product.CodeStateConflict, "invalid model attempt outcome")
	}
	if status == "accepted" && (msg == nil || msg.Status != agent.StatusComplete) {
		return nil, product.NewError(product.CodeInvalidArgument, "accepted attempt requires complete message")
	}
	var controls []store.Record
	if msg != nil {
		expectedScope := agent.MessageScope{SessionID: scope.SessionID, TraceID: scope.TraceID, InvocationID: scope.InvocationID}
		if msg.ID != initial.MessageID || msg.Scope != expectedScope {
			return nil, product.NewError(product.CodeStateConflict, "child candidate identity mismatch")
		}
		if err := msg.Validate(); err != nil {
			return nil, err
		}
		if msg.Kind != agent.KindAssistant || msg.Source.Kind != agent.SourceModel || msg.Standard.Role != schema.AgenticRoleTypeAssistant || status != "accepted" && msg.Status != agent.StatusIncomplete {
			return nil, product.NewError(product.CodeInvalidArgument, "child candidate must be an assistant with the terminal status")
		}
		if _, exists := v.InvocationMessages[msg.ID]; exists {
			return nil, product.NewError(product.CodeStateConflict, "child candidate already recorded")
		}
		controls = append(controls, record("invocation_message", msg.ID, *msg))
	}
	if status != "accepted" {
		if len(calls) != 0 {
			return nil, product.NewError(product.CodeInvalidArgument, "failed attempt cannot accept tools")
		}
		return controls, nil
	}
	// The provider's tool sequence is authoritative. Product identities must
	// pair with it exactly, including arguments, without reordering or reuse.
	auxiliary := initial.Purpose == "compaction"
	if auxiliary && len(calls) != 0 {
		return nil, product.NewError(product.CodeInvalidArgument, "auxiliary model response cannot admit tools")
	}
	index := 0
	providers := map[string]bool{}
	for _, block := range msg.Standard.ContentBlocks {
		if block == nil {
			return nil, product.NewError(product.CodeInvalidArgument, "invalid child content block")
		}
		if block.Type != schema.ContentBlockTypeFunctionToolCall {
			if block.FunctionToolCall != nil {
				return nil, product.NewError(product.CodeStateConflict, "child tool block type mismatch")
			}
			continue
		}
		if auxiliary {
			return nil, product.NewError(product.CodeInvalidArgument, "auxiliary model response cannot admit tools")
		}
		tool := block.FunctionToolCall
		if tool == nil || index >= len(calls) {
			return nil, product.NewError(product.CodeStateConflict, "child candidate tool count mismatch")
		}
		call := calls[index]
		if call.Scope != scope || tool.CallID == "" || providers[tool.CallID] || tool.CallID != call.Call.ProviderCallID || tool.Name != call.Call.Name || tool.Arguments != call.Call.Arguments || !json.Valid([]byte(tool.Arguments)) {
			return nil, product.NewError(product.CodeStateConflict, "child candidate tool identity mismatch")
		}
		providers[tool.CallID] = true
		index++
	}
	if index != len(calls) {
		return nil, product.NewError(product.CodeStateConflict, "child candidate tool count mismatch")
	}
	inv, callRecords, err := childCallRecords(v, inv, calls)
	if err != nil {
		return nil, err
	}
	inv.MessageIDs = append(append([]string(nil), inv.MessageIDs...), msg.ID)
	controls = append(controls, callRecords...)
	controls = append(controls, record("invocation", inv.ID, inv))
	return controls, nil
}

func (m *Manager) saveChildAttemptResult(ctx context.Context, initial ModelAttempt, result ModelAttemptTransition, msg *agent.AgentMessage, calls []agent.ToolRecord, controls []store.Record) error {
	if initial.Scope.SessionID != m.sessionID {
		return product.NewError(product.CodeStateConflict, "child attempt session mismatch")
	}
	private, err := childAttemptRecords(m.view, initial, result.State, msg, calls)
	if err != nil {
		return err
	}
	controls = append(controls, private...)
	// Only immutable identities and state are display facts. Child text, tool
	// arguments, provider Extra and terminal diagnostics stay private.
	payload := struct {
		AttemptID    string `json:"attemptId"`
		MessageID    string `json:"messageId"`
		InvocationID string `json:"invocationId"`
		State        string `json:"state"`
	}{initial.ID, initial.MessageID, initial.Scope.InvocationID, result.State}
	_, err = m.commit(ctx, controls, nil, []agent.Event{m.event("model.attempt_finalized", initial.Scope.TraceID, "", payload)})
	return err
}

func applyInvocationMessage(v *View, r store.Record) error {
	var msg agent.AgentMessage
	if err := json.Unmarshal(r.Payload, &msg); err != nil {
		return err
	}
	if err := msg.Validate(); err != nil {
		return err
	}
	return putImmutable(&v.InvocationMessages, r.ID, msg.ID, msg)
}

// validateChildAttemptCommit checks the complete transaction before any record
// is applied, both on live candidates and load-only replay. Legacy child call
// batches and old failed attempts without private records remain unchanged.
func validateChildAttemptCommit(v *View, c store.Commit) error {
	var transitions []ModelAttemptTransition
	privateCommit := false
	for _, r := range c.ControlRecords {
		switch r.Type {
		case "invocation_message":
			privateCommit = true
		case "invocation":
			var inv Invocation
			if err := json.Unmarshal(r.Payload, &inv); err != nil {
				return err
			}
			if !reflect.DeepEqual(inv.MessageIDs, v.Invocations[inv.ID].MessageIDs) {
				privateCommit = true
			}
		case "model_attempt_transition":
			var result ModelAttemptTransition
			if err := json.Unmarshal(r.Payload, &result); err != nil {
				return err
			}
			initial := v.ModelAttempts[result.AttemptID]
			if initial.Scope.ParentInvocationID != "" {
				transitions = append(transitions, result)
				privateCommit = privateCommit || result.State == "accepted" && initial.Scope.TurnID == ""
			}
		}
	}
	for _, event := range c.Events {
		privateCommit = privateCommit || event.Type == "model.attempt_finalized"
	}
	if !privateCommit {
		return nil
	}
	if len(transitions) != 1 || len(c.Entries) != 0 || len(c.BranchUpdates) != 0 {
		return product.NewError(product.CodeIncompatibleVersion, "partial child attempt commit")
	}
	result := transitions[0]
	initial := v.ModelAttempts[result.AttemptID]
	var msg *agent.AgentMessage
	var calls []agent.ToolRecord
	var private []store.Record
	var nextInvocation Invocation
	var details ModelAttemptDetailsRecord
	detailsCount, transitionCount := 0, 0
	for _, r := range c.ControlRecords {
		if r.Version != 1 || r.ParentID != "" {
			return product.NewError(product.CodeIncompatibleVersion, "invalid child control record")
		}
		switch r.Type {
		case "model_attempt_details":
			if err := json.Unmarshal(r.Payload, &details); err != nil {
				return err
			}
			detailsCount++
		case "model_attempt_transition":
			transitionCount++
		case "invocation_message":
			if msg != nil {
				return product.NewError(product.CodeIncompatibleVersion, "duplicate child candidate")
			}
			msg = new(agent.AgentMessage)
			if err := json.Unmarshal(r.Payload, msg); err != nil {
				return err
			}
			private = append(private, r)
		case "tool_call":
			var call agent.ToolRecord
			if err := json.Unmarshal(r.Payload, &call); err != nil {
				return err
			}
			if r.ID != call.Call.CallID {
				return product.NewError(product.CodeIncompatibleVersion, "child call record identity mismatch")
			}
			calls = append(calls, call)
			private = append(private, r)
		case "invocation":
			if err := json.Unmarshal(r.Payload, &nextInvocation); err != nil {
				return err
			}
			private = append(private, r)
		default:
			return product.NewError(product.CodeIncompatibleVersion, "unexpected child attempt control")
		}
	}
	if detailsCount != 1 || transitionCount != 1 || details.AttemptID != initial.ID || result.UsageRef != details.ID || result.DiagnosticRef != details.ID {
		return product.NewError(product.CodeIncompatibleVersion, "child attempt evidence is not atomic")
	}
	expected, err := childAttemptRecords(v, initial, result.State, msg, calls)
	if err != nil {
		return err
	}
	if len(expected) != len(private) {
		return product.NewError(product.CodeIncompatibleVersion, "child attempt acceptance is incomplete")
	}
	for i, r := range expected {
		if r.Type != private[i].Type || r.ID != private[i].ID {
			return product.NewError(product.CodeIncompatibleVersion, "child attempt acceptance order differs")
		}
		if r.Type == "invocation" {
			var inv Invocation
			if err := json.Unmarshal(r.Payload, &inv); err != nil {
				return err
			}
			if !reflect.DeepEqual(inv, nextInvocation) {
				return product.NewError(product.CodeIncompatibleVersion, "child attempt invocation projection differs")
			}
		}
	}
	if len(c.Events) != 1 {
		return product.NewError(product.CodeIncompatibleVersion, "child attempt terminal event is missing")
	}
	event := c.Events[0]
	var payload map[string]string
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return err
	}
	want := map[string]string{"attemptId": initial.ID, "messageId": initial.MessageID, "invocationId": initial.Scope.InvocationID, "state": result.State}
	if event.Type != "model.attempt_finalized" || event.Scope != (agent.EventScope{SessionID: initial.Scope.SessionID, TraceID: initial.Scope.TraceID}) || !reflect.DeepEqual(payload, want) {
		return product.NewError(product.CodeIncompatibleVersion, "child attempt terminal event is not safe")
	}
	return nil
}
