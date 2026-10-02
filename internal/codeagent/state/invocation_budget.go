package state

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	store "github.com/ww1489/seasprak/internal/storage"
)

// InvocationBudget is an independently cumulative child model ledger. Tool
// claims belong to the trace ledger, never to this private model reservation.
type InvocationBudget struct {
	ID    string                            `json:"id"`
	Scope agent.ExecutionScope              `json:"scope"`
	Usage agent.Usage                       `json:"usage"`
	Calls map[string]InvocationModelRequest `json:"calls"`
}

// InvocationModelRequest retains per-call reservations when model nodes or a
// summary interleave. Usage remains the invocation's aggregate occupancy.
type InvocationModelRequest struct {
	Requests      int                  `json:"requests"`
	LastTransport llm.TransportRequest `json:"lastTransport"`
}

const invocationBudgetEvent = "model.invocation_budget_reserved"

func (m *Manager) SaveInvocationBudget(ctx context.Context, scope agent.ExecutionScope, child, parent agent.Usage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if scope.SessionID != m.sessionID {
		return product.NewError(product.CodeStateConflict, "child budget session mismatch")
	}
	budget, trace, err := invocationBudgetCandidate(m.view, scope, child, parent)
	if err != nil {
		return err
	}
	_, err = m.commit(ctx, []store.Record{record("invocation_budget", budget.ID, budget), record("trace", trace.ID, trace)}, nil,
		[]agent.Event{m.event(invocationBudgetEvent, scope.TraceID, "", invocationBudgetPayload(budget, parent))})
	return err
}

// The parent fields are exact compare-and-set candidates. In particular,
// reserving a child request cannot advance or replace a parent model Turn.
func invocationBudgetCandidate(v *View, scope agent.ExecutionScope, used, parentUsage agent.Usage) (InvocationBudget, TraceState, error) {
	next := InvocationBudget{ID: scope.InvocationID, Scope: scope, Usage: used}
	inv, found := v.Invocations[next.ID]
	tr := v.Traces[scope.TraceID]
	parent, claimed := v.Calls[inv.ParentCallID]
	if !found || next.ID != scope.InvocationID || inv.State != "running" || inv.TraceID != scope.TraceID || inv.ParentInvocationID != scope.ParentInvocationID || scope.ParentInvocationID == "" || scope.TurnID != "" || scope.SelectionRevision != 0 || scope.ExecutionID == "" || scope.BranchID != v.BranchID ||
		tr == nil || tr.State != "running" || tr.Settled || tr.ExecutionStopped || scope.Generation != tr.Generation || inv.Target.Generation != scope.Generation || !claimed || !parent.Claimed || parent.Observation != nil || parent.Scope.SessionID != scope.SessionID || parent.Scope.BranchID != scope.BranchID || parent.Scope.TraceID != scope.TraceID || parent.Scope.InvocationID != scope.ParentInvocationID || parent.Scope.Generation != scope.Generation {
		return InvocationBudget{}, TraceState{}, product.NewError(product.CodeStateConflict, "child budget requires its active invocation and claimed parent")
	}
	if scope.ExecutionID != parent.Scope.ExecutionID {
		segment, mapped := v.ResumedExecutions[scope.ExecutionID]
		expected := agent.ExecutionScope{SessionID: scope.SessionID, BranchID: scope.BranchID, TraceID: scope.TraceID, InvocationID: tr.InvocationID, ExecutionID: scope.ExecutionID, Generation: scope.Generation}
		if !mapped || segment.InvocationID != inv.ID || segment.ParentCallID != inv.ParentCallID || segment.Scope != expected || tr.ExecutionID != scope.ExecutionID {
			return InvocationBudget{}, TraceState{}, product.NewError(product.CodeStateConflict, "child budget execution has no original recovery mapping")
		}
	}
	old, exists := v.InvocationBudgets[next.ID]
	if exists {
		original := old.Scope
		original.ExecutionID = scope.ExecutionID
		if original != scope || old.Scope.ExecutionID != scope.ExecutionID && scope.ExecutionID == parent.Scope.ExecutionID {
			return InvocationBudget{}, TraceState{}, product.NewError(product.CodeStateConflict, "child budget scope is immutable outside a mapped recovery")
		}
	}
	prior := old.Usage
	next.Calls = maps.Clone(old.Calls)
	if next.Calls == nil {
		next.Calls = make(map[string]InvocationModelRequest)
	}
	progress, reserved := next.Calls[used.ModelCallID]
	if used.LogicalModelCalls < prior.LogicalModelCalls || used.TransportRequests < prior.TransportRequests || used.ToolExecutions != 0 || used.ModelRequests < 0 {
		return InvocationBudget{}, TraceState{}, product.NewError(product.CodeStateConflict, "child model occupancy cannot be refunded or claim tools")
	}
	logical, physical := used.LogicalModelCalls-prior.LogicalModelCalls, used.TransportRequests-prior.TransportRequests
	switch {
	case logical == 1 && physical == 0:
		expected := prior
		expected.LogicalModelCalls++
		expected.ModelCallID, expected.ModelRequests, expected.LastTransport = used.ModelCallID, 0, llm.TransportRequest{}
		if used != expected || used.ModelCallID == "" || reserved || used.ModelCallID == prior.ModelCallID || used.ModelCallID == tr.Usage.ModelCallID {
			return InvocationBudget{}, TraceState{}, product.NewError(product.CodeStateConflict, "invalid child logical model reservation")
		}
		for _, attempt := range v.ModelAttempts {
			if attempt.ModelCallID == used.ModelCallID {
				return InvocationBudget{}, TraceState{}, product.NewError(product.CodeStateConflict, "child logical identity was already used")
			}
		}
	case logical == 0 && physical == 1:
		expected := prior
		expected.TransportRequests++
		expected.ModelCallID, expected.ModelRequests = used.ModelCallID, progress.Requests+1
		expected.LastTransport = used.LastTransport
		if !exists || !reserved || old.Scope != scope || used != expected || used.ModelCallID == "" {
			return InvocationBudget{}, TraceState{}, product.NewError(product.CodeStateConflict, "invalid child physical model reservation")
		}
		request := used.LastTransport
		if request == (llm.TransportRequest{}) {
			// The existing injected-model path reserves a call without claiming
			// an observed wire request. Its started attempt is still mandatory.
			active := 0
			for id, attempt := range v.ModelAttempts {
				if _, ended := v.AttemptResults[id]; !ended && attempt.State == "started" && attempt.Scope == scope && attempt.ModelCallID == used.ModelCallID {
					active++
				}
			}
			if active != 1 {
				return InvocationBudget{}, TraceState{}, product.NewError(product.CodeStateConflict, "child model reservation has no unique started attempt")
			}
		} else {
			attempt, registered := v.ModelAttempts[request.AttemptID]
			_, ended := v.AttemptResults[request.AttemptID]
			ordinal := uint64(1)
			if progress.LastTransport.AttemptID == request.AttemptID {
				ordinal = progress.LastTransport.TransportAttempt + 1
			}
			if !registered || ended || attempt.State != "started" || attempt.Scope != scope || attempt.ModelCallID != used.ModelCallID || request.ModelCallID != used.ModelCallID || request.Purpose == "" || request.TransportAttempt != ordinal || ordinal == 0 {
				return InvocationBudget{}, TraceState{}, product.NewError(product.CodeStateConflict, "child transport does not belong to its original active attempt")
			}
		}
	default:
		return InvocationBudget{}, TraceState{}, product.NewError(product.CodeStateConflict, "child budget must reserve exactly one logical call or request")
	}
	limits := tr.Limits
	if logical > limits.TraceLogicalModelCalls-tr.Usage.LogicalModelCalls || physical > limits.TraceTransportRequests-tr.Usage.TransportRequests || used.ModelRequests > limits.LogicalModelRequests {
		return InvocationBudget{}, TraceState{}, product.NewError(product.CodeBudgetExhausted, "trace model budget exhausted")
	}
	expectedParent := tr.Usage
	expectedParent.LogicalModelCalls += logical
	expectedParent.TransportRequests += physical
	if parentUsage != expectedParent {
		return InvocationBudget{}, TraceState{}, product.NewError(product.CodeStateConflict, "parent aggregate does not match the child reservation")
	}
	next.Calls[used.ModelCallID] = InvocationModelRequest{Requests: used.ModelRequests, LastTransport: used.LastTransport}
	trace := *tr
	trace.Usage = expectedParent
	return next, trace, nil
}

func invocationBudgetPayload(budget InvocationBudget, parent agent.Usage) any {
	return struct {
		InvocationID      string               `json:"invocationId"`
		ModelCallID       string               `json:"modelCallId"`
		LogicalCalls      int                  `json:"logicalCalls"`
		TransportRequests int                  `json:"transportRequests"`
		LogicalRequests   int                  `json:"logicalRequests"`
		TraceLogicalCalls int                  `json:"traceLogicalCalls"`
		TraceRequests     int                  `json:"traceRequests"`
		Request           llm.TransportRequest `json:"request"`
	}{budget.ID, budget.Usage.ModelCallID, budget.Usage.LogicalModelCalls, budget.Usage.TransportRequests, budget.Usage.ModelRequests, parent.LogicalModelCalls, parent.TransportRequests, budget.Usage.LastTransport}
}

// The marker is checked as well as the budget record, so deleting either half
// of a joint reservation cannot silently become a legacy trace-only update.
func validateInvocationBudgetCommit(v *View, c store.Commit) error {
	marked := false
	for _, r := range c.ControlRecords {
		marked = marked || r.Type == "invocation_budget"
	}
	for _, event := range c.Events {
		marked = marked || event.Type == invocationBudgetEvent
	}
	if !marked {
		return nil
	}
	if len(c.ControlRecords) != 2 || len(c.Entries) != 0 || len(c.BranchUpdates) != 0 || len(c.Events) != 1 {
		return product.NewError(product.CodeIncompatibleVersion, "partial child budget reservation")
	}
	var budget InvocationBudget
	var trace TraceState
	budgets, traces := 0, 0
	for _, r := range c.ControlRecords {
		if r.Version != 1 || r.ParentID != "" {
			return product.NewError(product.CodeIncompatibleVersion, "invalid child budget control record")
		}
		switch r.Type {
		case "invocation_budget":
			if err := json.Unmarshal(r.Payload, &budget); err != nil {
				return err
			}
			if r.ID != budget.ID {
				return product.NewError(product.CodeIncompatibleVersion, "child budget record identity differs")
			}
			budgets++
		case "trace":
			if err := json.Unmarshal(r.Payload, &trace); err != nil {
				return err
			}
			if r.ID != trace.ID {
				return product.NewError(product.CodeIncompatibleVersion, "parent budget record identity differs")
			}
			traces++
		default:
			return product.NewError(product.CodeIncompatibleVersion, "unexpected child budget control")
		}
	}
	if budgets != 1 || traces != 1 || trace.ID != budget.Scope.TraceID {
		return product.NewError(product.CodeIncompatibleVersion, "child and parent budget reservation are not paired")
	}
	expectedBudget, expected, err := invocationBudgetCandidate(v, budget.Scope, budget.Usage, trace.Usage)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expectedBudget, budget) {
		return product.NewError(product.CodeIncompatibleVersion, "child per-call reservations differ from the original progress")
	}
	if !reflect.DeepEqual(expected, trace) {
		return product.NewError(product.CodeIncompatibleVersion, "parent trace changed outside the child budget reservation")
	}
	event := c.Events[0]
	var actualPayload, expectedPayload any
	raw, err := json.Marshal(invocationBudgetPayload(budget, trace.Usage))
	if err != nil {
		return err
	}
	if json.Unmarshal(event.Payload, &actualPayload) != nil || json.Unmarshal(raw, &expectedPayload) != nil || event.SchemaVersion != 1 || event.Type != invocationBudgetEvent || event.Scope != (agent.EventScope{SessionID: budget.Scope.SessionID, TraceID: budget.Scope.TraceID}) || !reflect.DeepEqual(actualPayload, expectedPayload) {
		return product.NewError(product.CodeIncompatibleVersion, "child budget reservation event differs or contains private data")
	}
	return nil
}

func applyInvocationBudget(v *View, r store.Record) error {
	var budget InvocationBudget
	if err := json.Unmarshal(r.Payload, &budget); err != nil {
		return err
	}
	if v.InvocationBudgets == nil {
		v.InvocationBudgets = make(map[string]InvocationBudget)
	}
	v.InvocationBudgets[budget.ID] = budget
	return nil
}
