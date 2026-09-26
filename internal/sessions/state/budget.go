package state

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

func (m *Manager) SaveTraceBudget(ctx context.Context, id string, usage agent.Usage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[id]
	if tr == nil {
		return product.NewError(product.CodeNotFound, "trace not found")
	}
	old := tr.Usage
	if usage.LogicalModelCalls < old.LogicalModelCalls || usage.TransportRequests < old.TransportRequests || usage.ToolExecutions < old.ToolExecutions || usage.ModelRequests < 0 {
		return product.NewError(product.CodeStateConflict, "durable budget cannot be refunded")
	}
	if usage.ModelCallID == old.ModelCallID {
		if usage.ModelRequests < old.ModelRequests {
			return product.NewError(product.CodeStateConflict, "model requests cannot be refunded")
		}
	} else if usage.ModelCallID == "" || usage.LogicalModelCalls != old.LogicalModelCalls+1 || usage.ModelRequests != 0 || usage.TransportRequests != old.TransportRequests {
		return product.NewError(product.CodeStateConflict, "invalid logical model budget transition")
	}
	if usage.ModelCallID == old.ModelCallID && usage.TransportRequests == old.TransportRequests && usage.LastTransport != old.LastTransport {
		return product.NewError(product.CodeStateConflict, "transport identity requires a new reservation")
	}
	if usage.ModelCallID != old.ModelCallID && usage.LastTransport != (llm.TransportRequest{}) {
		return product.NewError(product.CodeStateConflict, "new logical call cannot inherit transport identity")
	}
	if usage.LogicalModelCalls > tr.Limits.TraceLogicalModelCalls || usage.TransportRequests > tr.Limits.TraceTransportRequests || usage.ToolExecutions > tr.Limits.TraceToolCalls || usage.ModelRequests > tr.Limits.LogicalModelRequests {
		return product.NewError(product.CodeBudgetExhausted, "trace budget exhausted")
	}
	next := *tr
	next.Usage = usage
	controls := []store.Record{record("trace", id, next)}
	var events []agent.Event
	if usage.ModelCallID != "" {
		turn, exists := m.view.Turns[usage.ModelCallID]
		if exists && (turn.TraceID != id || turn.InvocationID != tr.InvocationID || turn.Ended) {
			return product.NewError(product.CodeStateConflict, "model call is not active")
		}
		if !exists {
			turn = agent.TurnRecord{ID: usage.ModelCallID, TraceID: id, InvocationID: tr.InvocationID}
			events = append(events, m.event("turn_start", id, turn.ID, turn))
		}
		turn.TransportRequests = usage.ModelRequests
		controls = append(controls, record("turn", turn.ID, turn))
	}
	if usage.TransportRequests > old.TransportRequests && usage.LastTransport != (llm.TransportRequest{}) {
		request := usage.LastTransport
		if usage.TransportRequests != old.TransportRequests+1 || usage.ModelRequests != old.ModelRequests+1 || usage.ModelCallID != old.ModelCallID || request.ModelCallID != usage.ModelCallID || request.AttemptID == "" || request.Purpose == "" || request.TransportAttempt == 0 {
			return product.NewError(product.CodeStateConflict, "invalid physical request budget transition")
		}
		// Reservation and identity are durable in the same commit. A reservation
		// does not claim the wire request started (cancellation may follow).
		events = append(events, m.event("model.transport_reserved", id, usage.ModelCallID, struct {
			Request        llm.TransportRequest `json:"request"`
			LogicalRequest int                  `json:"logicalRequest"`
			TraceRequest   int                  `json:"traceRequest"`
		}{request, usage.ModelRequests, usage.TransportRequests}))
	}
	_, err := m.commit(ctx, controls, nil, events)
	return err
}
func (m *Manager) ClaimTool(ctx context.Context, frozen agent.FrozenCall, usage agent.Usage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	_, err := m.commit(ctx, []store.Record{record("trace", tr.ID, next), record("tool_call", frozen.CallID, call)}, nil, []agent.Event{m.event("tool.state_changed", tr.ID, call.Scope.TurnID, call)})
	return err
}

func (m *Manager) SaveBudget(ctx context.Context, usage agent.Usage) error {
	v := m.View()
	if v.ActiveTrace == "" {
		return product.NewError(product.CodeStateConflict, "no active trace")
	}
	return m.SaveTraceBudget(ctx, v.ActiveTrace, usage)
}
