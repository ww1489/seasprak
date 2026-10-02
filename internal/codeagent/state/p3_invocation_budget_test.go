package state_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	store "github.com/ww1489/seasprak/internal/storage"
)

// Use the actual Manager method through this port so the missing capability
// fails as behavior, rather than preventing unrelated packages from building.
type invocationBudgetWriter interface {
	SaveInvocationBudget(context.Context, agent.ExecutionScope, agent.Usage, agent.Usage) error
}

func p3InvocationBudgetFixture(t *testing.T) (*state.Manager, *failingStore, invocationBudgetWriter, agent.ExecutionScope, agent.Usage) {
	t.Helper()
	m, backend, initial, _, _ := childAttemptFixture(t)
	writer, ok := any(m).(invocationBudgetWriter)
	if !ok {
		t.Fatal("Manager cannot atomically persist an independent invocation budget with the parent aggregate")
	}
	parent := agent.NewBudget(config.Limits{})
	parent.SetPersist(func(u agent.Usage) error { return m.SaveTraceBudget(t.Context(), initial.Scope.TraceID, u) })
	if err := parent.BeginTurnID("parent-turn"); err != nil {
		t.Fatal(err)
	}
	if err := parent.BeforeRequest(t.Context(), llm.TransportRequest{RequestIdentity: llm.RequestIdentity{ModelCallID: "parent-turn", AttemptID: "parent-attempt", Purpose: "agent"}, TransportAttempt: 1}); err != nil {
		t.Fatal(err)
	}
	return m, backend, writer, initial.Scope, parent.Snapshot()
}

func p3InvocationLogical(t *testing.T, m *state.Manager, writer invocationBudgetWriter, scope agent.ExecutionScope, parent agent.Usage) (agent.Usage, agent.Usage, state.ModelAttempt) {
	t.Helper()
	child := agent.Usage{LogicalModelCalls: 1, ModelCallID: "budget-call"}
	parent.LogicalModelCalls++
	if err := writer.SaveInvocationBudget(t.Context(), scope, child, parent); err != nil {
		t.Fatal(err)
	}
	attempt := state.ModelAttempt{ID: "budget-attempt", ModelCallID: child.ModelCallID, MessageID: "budget-candidate", StreamID: "budget-stream", Scope: scope, Purpose: "agent", Attempt: 1, State: "started", ModelConfigVersion: "synthetic-v1"}
	if err := m.SaveRecords(t.Context(), m.View().LastSeq, state.Records{ModelAttempts: []state.ModelAttempt{attempt}}); err != nil {
		t.Fatal(err)
	}
	return child, parent, attempt
}

func p3InvocationPhysical(child, parent agent.Usage, attempt state.ModelAttempt) (agent.Usage, agent.Usage) {
	child.TransportRequests++
	child.ModelRequests++
	child.LastTransport = llm.TransportRequest{RequestIdentity: llm.RequestIdentity{ModelCallID: child.ModelCallID, AttemptID: attempt.ID, Purpose: "agent"}, TransportAttempt: 1}
	parent.TransportRequests++
	return child, parent
}

func TestP3InvocationBudgetAtomicIdentityAndReplay(t *testing.T) {
	m, backend, writer, scope, parent := p3InvocationBudgetFixture(t)
	before := m.View()
	child, parent, attempt := p3InvocationLogical(t, m, writer, scope, parent)
	child, parent = p3InvocationPhysical(child, parent, attempt)
	if err := writer.SaveInvocationBudget(t.Context(), scope, child, parent); err != nil {
		t.Fatal(err)
	}
	after := m.View()
	requireChildParentUnchanged(t, before, after)
	if after.Traces[scope.TraceID].Usage != parent || parent.ModelCallID != before.Traces[scope.TraceID].Usage.ModelCallID || parent.ModelRequests != 1 || parent.LastTransport != before.Traces[scope.TraceID].Usage.LastTransport {
		t.Fatal("child reservation replaced parent per-call request identity")
	}
	stored, err := backend.Load(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	commit := stored.Commits[len(stored.Commits)-1]
	budgets, traces := 0, 0
	for _, r := range commit.ControlRecords {
		switch r.Type {
		case "invocation_budget":
			var item struct {
				ID    string               `json:"id"`
				Scope agent.ExecutionScope `json:"scope"`
				Usage agent.Usage          `json:"usage"`
			}
			if json.Unmarshal(r.Payload, &item) != nil || r.ID != scope.InvocationID || item.ID != r.ID || item.Scope != scope || item.Usage != child {
				t.Fatal("reservation lost independent identity, cumulative usage or original transport")
			}
			budgets++
		case "trace":
			traces++
		default:
			t.Fatal("child reservation wrote non-budget control facts")
		}
	}
	if budgets != 1 || traces != 1 || len(commit.Entries) != 0 {
		t.Fatal("child reservation and parent aggregate are not in one transaction")
	}
	reopened, err := state.NewManager(backend, "session")
	if err != nil || !reflect.DeepEqual(after, reopened.View()) {
		t.Fatal("read-only replay refunded or changed independent budget", err)
	}
}

func TestP3InvocationBudgetInterleavedLogicalCallsKeepIndependentRequests(t *testing.T) {
	m, backend, writer, scope, parent := p3InvocationBudgetFixture(t)
	before := m.View()
	child, parent, first := p3InvocationLogical(t, m, writer, scope, parent)
	child.LogicalModelCalls++
	child.ModelCallID, child.ModelRequests, child.LastTransport = "parallel-call", 0, llm.TransportRequest{}
	parent.LogicalModelCalls++
	if err := writer.SaveInvocationBudget(t.Context(), scope, child, parent); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID, second.ModelCallID, second.MessageID, second.StreamID = "parallel-attempt", child.ModelCallID, "parallel-candidate", "parallel-stream"
	if err := m.SaveRecords(t.Context(), m.View().LastSeq, state.Records{ModelAttempts: []state.ModelAttempt{second}}); err != nil {
		t.Fatal(err)
	}
	for i, attempt := range []state.ModelAttempt{first, second, first} {
		ordinal := uint64(1)
		if i == 2 {
			ordinal = 2
		}
		child.TransportRequests++
		child.ModelCallID, child.ModelRequests = attempt.ModelCallID, int(ordinal)
		child.LastTransport = llm.TransportRequest{RequestIdentity: llm.RequestIdentity{ModelCallID: attempt.ModelCallID, AttemptID: attempt.ID, Purpose: "agent"}, TransportAttempt: ordinal}
		parent.TransportRequests++
		if err := writer.SaveInvocationBudget(t.Context(), scope, child, parent); err != nil {
			t.Fatalf("interleaved request %d must keep the original logical identity and its own ordinal: %v", i+1, err)
		}
	}
	after := m.View()
	requireChildParentUnchanged(t, before, after)
	if after.InvocationBudgets[scope.InvocationID].Usage != child || after.InvocationBudgets[scope.InvocationID].Calls[first.ModelCallID].Requests != 2 || after.InvocationBudgets[scope.InvocationID].Calls[second.ModelCallID].Requests != 1 || after.InvocationBudgets[scope.InvocationID].Calls[first.ModelCallID].LastTransport.AttemptID != first.ID || after.InvocationBudgets[scope.InvocationID].Calls[second.ModelCallID].LastTransport.AttemptID != second.ID || child.LogicalModelCalls != 2 || child.TransportRequests != 3 || child.ModelRequests != 2 || after.Traces[scope.TraceID].Usage != parent || parent.ModelCallID != before.Traces[scope.TraceID].Usage.ModelCallID || parent.LastTransport != before.Traces[scope.TraceID].Usage.LastTransport {
		t.Fatal("parallel requests lost independent counts or changed the parent's request")
	}
	reopened, err := state.NewManager(backend, "session")
	if err != nil || !reflect.DeepEqual(after, reopened.View()) {
		t.Fatal("load-only replay lost an overlapping logical call reservation", err)
	}
}

func TestP3InvocationBudgetRejectsInvalidCandidatesWithoutAppend(t *testing.T) {
	for _, name := range []string{"parent-progress", "parent-refund", "child-refund", "child-tools", "request-attempt", "request-ordinal", "request-call", "request-ended", "scope-execution", "scope-parent", "scope-session", "scope-branch", "scope-generation", "scope-turn", "scope-selection", "child-stopped", "trace-cancelling"} {
		t.Run(name, func(t *testing.T) {
			m, _, writer, scope, parent := p3InvocationBudgetFixture(t)
			child, parent, attempt := p3InvocationLogical(t, m, writer, scope, parent)
			child, parent = p3InvocationPhysical(child, parent, attempt)
			switch name {
			case "parent-progress":
				parent.ModelRequests++
			case "parent-refund":
				parent.TransportRequests--
			case "child-refund":
				child.LogicalModelCalls--
			case "child-tools":
				child.ToolExecutions++
			case "request-attempt":
				child.LastTransport.AttemptID = "foreign"
			case "request-ordinal":
				child.LastTransport.TransportAttempt++
			case "request-call":
				child.LastTransport.ModelCallID = "parent-turn"
			case "request-ended":
				if err := m.SaveAttemptResult(t.Context(), scope, attempt.ID, "aborted", "", nil, nil); err != nil {
					t.Fatal(err)
				}
			case "scope-execution":
				scope.ExecutionID = "unmapped"
			case "scope-parent":
				scope.ParentInvocationID = "foreign"
			case "scope-session":
				scope.SessionID = "foreign"
			case "scope-branch":
				scope.BranchID = "foreign"
			case "scope-generation":
				scope.Generation = "foreign"
			case "scope-turn":
				scope.TurnID = "parent-turn"
			case "scope-selection":
				scope.SelectionRevision = 1
			case "child-stopped":
				inv, _ := m.Invocation(scope.InvocationID)
				inv.State = "interrupted"
				if err := m.SaveInvocation(t.Context(), inv); err != nil {
					t.Fatal(err)
				}
			case "trace-cancelling":
				if err := m.SetTraceState(t.Context(), scope.TraceID, "cancelling", false); err != nil {
					t.Fatal(err)
				}
			}
			before := m.View()
			err := writer.SaveInvocationBudget(t.Context(), scope, child, parent)
			requireP2Code(t, err, product.CodeStateConflict)
			if !reflect.DeepEqual(before, m.View()) || m.Fault() != nil {
				t.Fatal("rejected budget candidate changed state or faulted manager")
			}
		})
	}
}

func TestP3InvocationBudgetAppendFailureKeepsBothCandidatesPrivate(t *testing.T) {
	m, backend, writer, scope, parent := p3InvocationBudgetFixture(t)
	child, parent, attempt := p3InvocationLogical(t, m, writer, scope, parent)
	child, parent = p3InvocationPhysical(child, parent, attempt)
	before := m.View()
	backend.fail = true
	err := writer.SaveInvocationBudget(t.Context(), scope, child, parent)
	if err == nil || err.Error() != "injected sync failure" {
		t.Fatal("reservation did not propagate the actual store failure", err)
	}
	requireP2Code(t, m.WriteStatus(), product.CodeStorageUnavailable)
	if !reflect.DeepEqual(before, m.View()) || m.Fault() == nil {
		t.Fatal("failed append exposed one side of the reservation")
	}
	backend.fail = false
	reopened, err := state.NewManager(backend, "session")
	if err != nil || !reflect.DeepEqual(before, reopened.View()) {
		t.Fatal("failed reservation persisted after reopen", err)
	}
}

func TestP3InvocationBudgetReplayRejectsPartialOrModifiedReservation(t *testing.T) {
	for _, name := range []string{"missing-budget", "missing-trace", "changed-parent-usage", "changed-parent-state", "changed-child-usage", "changed-child-scope", "changed-request-attempt", "missing-call-progress", "changed-call-requests", "changed-call-transport", "foreign-call-progress", "duplicate-budget", "parent-turn", "parent-history", "missing-event", "unsafe-event"} {
		t.Run(name, func(t *testing.T) {
			m, backend, writer, scope, parent := p3InvocationBudgetFixture(t)
			child, parent, attempt := p3InvocationLogical(t, m, writer, scope, parent)
			child, parent = p3InvocationPhysical(child, parent, attempt)
			if err := writer.SaveInvocationBudget(t.Context(), scope, child, parent); err != nil {
				t.Fatal(err)
			}
			corrupt := childResumeMutationStore{Store: backend, mutate: func(c *store.Commit) {
				var controls []store.Record
				for _, r := range c.ControlRecords {
					if name == "missing-budget" && r.Type == "invocation_budget" || name == "missing-trace" && r.Type == "trace" {
						continue
					}
					if r.Type == "trace" {
						var trace state.TraceState
						_ = json.Unmarshal(r.Payload, &trace)
						if name == "changed-parent-usage" {
							trace.Usage.TransportRequests++
						}
						if name == "changed-parent-state" {
							trace.State = "completed"
						}
						r.Payload, _ = json.Marshal(trace)
					}
					if r.Type == "invocation_budget" {
						var item state.InvocationBudget
						_ = json.Unmarshal(r.Payload, &item)
						if name == "changed-child-usage" {
							item.Usage.ModelRequests++
						}
						if name == "changed-child-scope" {
							item.Scope.ExecutionID = "forged"
						}
						if name == "changed-request-attempt" {
							item.Usage.LastTransport.AttemptID = "forged"
						}
						switch name {
						case "missing-call-progress":
							delete(item.Calls, child.ModelCallID)
						case "changed-call-requests":
							progress := item.Calls[child.ModelCallID]
							progress.Requests++
							item.Calls[child.ModelCallID] = progress
						case "changed-call-transport":
							progress := item.Calls[child.ModelCallID]
							progress.LastTransport.AttemptID = "foreign"
							item.Calls[child.ModelCallID] = progress
						case "foreign-call-progress":
							item.Calls["foreign"] = state.InvocationModelRequest{}
						}
						r.Payload, _ = json.Marshal(item)
						if name == "duplicate-budget" {
							controls = append(controls, r)
						}
					}
					controls = append(controls, r)
				}
				c.ControlRecords = controls
				if name == "parent-turn" {
					c.ControlRecords = append(c.ControlRecords, store.Record{Type: "turn", Version: 1, ID: "forged", Payload: json.RawMessage(`{}`)})
				}
				if name == "parent-history" {
					c.Entries = append(c.Entries, store.Record{Type: "message", Version: 1, ID: "forged", Payload: json.RawMessage(`{}`)})
				}
				if name == "missing-event" {
					c.Events = nil
				}
				if name == "unsafe-event" {
					c.Events[0].Payload = json.RawMessage(`{"privateText":"forged"}`)
				}
			}}
			if _, err := state.NewManager(corrupt, "session"); err == nil {
				t.Fatal("partial or altered reservation was accepted by load-only replay")
			}
		})
	}
}
