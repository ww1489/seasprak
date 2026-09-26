package state

import (
	"context"
	"encoding/json"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

func (m *Manager) AppendEvent(ctx context.Context, kind, trace string, payload []byte) (store.CommitReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commit(ctx, nil, nil, []agent.Event{m.event(kind, trace, "", json.RawMessage(payload))})
}
func (m *Manager) AppendMessage(ctx context.Context, msg agent.AgentMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := msg.Validate(); err != nil {
		return err
	}
	entry := record("message", msg.ID, msg)
	entry.ParentID = m.view.LeafID
	_, err := m.commit(ctx, nil, []store.Record{entry}, []agent.Event{m.event("message.finalized", msg.Scope.TraceID, msg.Scope.TurnID, msg)})
	return err
}
func (m *Manager) SaveTurn(ctx context.Context, tr agent.TurnRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.view.Turns[tr.ID]; ok && old.Ended {
		return nil
	}
	kind := "turn_start"
	if tr.Ended {
		kind = "turn_end"
	}
	_, err := m.commit(ctx, []store.Record{record("turn", tr.ID, tr)}, nil, []agent.Event{m.event(kind, tr.TraceID, tr.ID, tr)})
	return err
}
func (m *Manager) SaveAssistant(ctx context.Context, msg agent.AgentMessage, calls []agent.ToolRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveAssistant(ctx, msg, calls, nil)
}
func (m *Manager) saveAssistant(ctx context.Context, msg agent.AgentMessage, calls []agent.ToolRecord, controls []store.Record) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	turn, ok := m.view.Turns[msg.Scope.TurnID]
	if !ok || turn.Ended || turn.TraceID != msg.Scope.TraceID {
		return product.NewError(product.CodeStateConflict, "assistant turn is not active")
	}
	events := []agent.Event{m.event("message.finalized", msg.Scope.TraceID, msg.Scope.TurnID, msg)}
	for _, call := range calls {
		controls = append(controls, record("tool_call", call.Call.CallID, call))
		turn.CallIDs = append(turn.CallIDs, call.Call.CallID)
		events = append(events, m.event("tool.requested", msg.Scope.TraceID, msg.Scope.TurnID, call))
	}
	controls = append(controls, record("turn", turn.ID, turn))
	entry := record("message", msg.ID, msg)
	entry.ParentID = m.view.LeafID
	_, err := m.commit(ctx, controls, []store.Record{entry}, events)
	return err
}
func (m *Manager) SaveCall(ctx context.Context, call agent.ToolRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.view.Calls[call.Call.CallID]
	if !ok {
		return product.NewError(product.CodeStateConflict, "tool call was not accepted")
	}
	if old.Call != call.Call || old.Scope != call.Scope || (old.Claimed && !call.Claimed) {
		return product.NewError(product.CodeStateConflict, "accepted tool identity is immutable")
	}
	if old.Observation != nil {
		if call.Observation == nil || *old.Observation != *call.Observation {
			return product.NewError(product.CodeStateConflict, "tool result already recorded")
		}
		return nil
	}
	kind := "tool.requested"
	if call.Claimed {
		kind = "tool.state_changed"
	}
	if call.Observation != nil {
		kind = "tool.finished"
	}
	_, err := m.commit(ctx, []store.Record{record("tool_call", call.Call.CallID, call)}, nil, []agent.Event{m.event(kind, call.Scope.TraceID, call.Scope.TurnID, call)})
	return err
}

// FinishTools persists results in the original assistant call order, independent of completion order.
func (m *Manager) FinishTools(ctx context.Context, turn agent.TurnRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.view.Turns[turn.ID]
	if old.Ended {
		return nil
	}
	var entries []store.Record
	parent := m.view.LeafID
	for _, id := range turn.CallIDs {
		call, ok := m.view.Calls[id]
		if !ok || call.Observation == nil {
			return product.NewError(product.CodeReconciliationRequired, "tool result is unresolved")
		}
		result := &schema.FunctionToolResult{CallID: call.Call.ProviderCallID, Name: call.Call.Name, Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: call.Observation.Content}}}}
		msg := agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindToolResult, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceTool}, Scope: agent.MessageScope{SessionID: m.sessionID, TraceID: turn.TraceID, TurnID: turn.ID, InvocationID: turn.InvocationID, ToolCallID: id}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(result)}}}
		entry := record("message", msg.ID, msg)
		entry.ParentID = parent
		parent = entry.ID
		entries = append(entries, entry)
	}
	turn.Ended = true
	events := []agent.Event{}
	for _, entry := range entries {
		events = append(events, m.event("message.finalized", turn.TraceID, turn.ID, entry.Payload))
	}
	events = append(events, m.event("turn_end", turn.TraceID, turn.ID, turn))
	_, err := m.commit(ctx, []store.Record{record("turn", turn.ID, turn)}, entries, events)
	return err
}
