package codeagent

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

// This test reads the actual durable transaction at the offline wire boundary.
// The two concurrent children must reserve independently, with the parent
// aggregate committed before either request can reach the physical transport.
func TestP3ChildRequestsPersistAtomicIdentityAndReopen(t *testing.T) {
	parent := newP3ChildAttemptModel(testkit.Step{ToolCalls: []schema.FunctionToolCall{
		{CallID: "root-a", Name: "delegate_task", Arguments: delegateArgs("a", "task-a")},
		{CallID: "root-b", Name: "delegate_task", Arguments: delegateArgs("b", "task-b")},
	}}, testkit.Step{Text: "parent final"})
	a, b := newP3ChildAttemptModel(testkit.Step{Text: "a final"}), newP3ChildAttemptModel(testkit.Step{Text: "b final"})
	opts := subagentOptions(agentRoots(t), "child-request-budget", parent.FakeModel,
		[]agent.AgentDefinition{{Name: "a", Version: "a1", Instruction: "A", Model: a, Delegable: true}, {Name: "b", Version: "b1", Instruction: "B", Model: b, Delegable: true}}, nil)
	opts.Model = parent
	s := openSubagentSession(t, opts, true)
	parent.manager, a.manager, b.manager = s.rt.manager, s.rt.manager, s.rt.manager
	var reserved atomic.Int32
	for _, child := range []*p3ChildAttemptModel{a, b} {
		child.beforeWire = func(r *http.Request) {
			scope := einorun.ScopeFromContext(r.Context(), agent.ExecutionScope{})
			stored, err := s.rt.opts.Store.Load(context.WithoutCancel(r.Context()), opts.SessionID)
			if err != nil {
				t.Error("cannot inspect committed reservation")
				return
			}
			found := false
			for i := len(stored.Commits) - 1; i >= 0 && !found; i-- {
				commit := stored.Commits[i]
				for _, record := range commit.ControlRecords {
					if record.Type != "invocation_budget" || record.ID != scope.InvocationID {
						continue
					}
					var childBudget struct {
						ID    string               `json:"id"`
						Scope agent.ExecutionScope `json:"scope"`
						Usage agent.Usage          `json:"usage"`
					}
					if json.Unmarshal(record.Payload, &childBudget) != nil {
						t.Error("invalid child reservation")
						return
					}
					usage := childBudget.Usage
					attempt := child.manager.View().ModelAttempts[usage.LastTransport.AttemptID]
					if childBudget.ID != scope.InvocationID || childBudget.Scope != scope || usage.LogicalModelCalls != 1 || usage.TransportRequests != int(child.physical.Load())+1 || usage.ModelRequests != usage.TransportRequests || usage.ToolExecutions != 0 || attempt.ID == "" || attempt.Scope != scope || attempt.ModelCallID != usage.ModelCallID || usage.LastTransport.ModelCallID != usage.ModelCallID || usage.LastTransport.TransportAttempt != uint64(usage.ModelRequests) || usage.LastTransport.Purpose == "" {
						t.Error("physical child request has no matching independent reservation and attempt identity")
						return
					}
					paired := false
					for _, control := range commit.ControlRecords {
						if control.Type == "turn" {
							t.Error("child reservation advanced a parent Turn")
							return
						}
						if control.Type != "trace" || control.ID != scope.TraceID {
							continue
						}
						var trace state.TraceState
						if json.Unmarshal(control.Payload, &trace) != nil {
							t.Error("invalid parent reservation")
							return
						}
						paired = trace.Usage.ModelCallID != usage.ModelCallID && trace.Usage.ModelRequests == 2 && trace.Usage.LastTransport.ModelCallID == trace.Usage.ModelCallID && trace.Usage.LastTransport.AttemptID != "" && trace.Usage.LastTransport.TransportAttempt == 2
					}
					if !paired || len(commit.Entries) != 0 {
						t.Error("child and parent occupancy were not committed atomically without altering parent progress")
						return
					}
					found = true
					reserved.Add(1)
				}
			}
			if !found {
				t.Error("physical child request started before its durable invocation reservation")
			}
		}
	}
	in := submitPrompt(t, s, "delegate twice")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	v := s.rt.manager.View()
	if v.Traces[in.TraceID].State != "completed" || reserved.Load() != 4 || a.physical.Load() != 2 || b.physical.Load() != 2 || parent.physical.Load() != 4 || v.Traces[in.TraceID].Usage.LogicalModelCalls != 4 || v.Traces[in.TraceID].Usage.TransportRequests != 8 {
		t.Fatal("concurrent child occupancy was lost or charged twice")
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s = openSubagentSession(t, opts, false)
	after := s.rt.manager.View()
	if !reflect.DeepEqual(v.Traces[in.TraceID].Usage, after.Traces[in.TraceID].Usage) || parent.physical.Load() != 4 || a.physical.Load() != 2 || b.physical.Load() != 2 {
		t.Fatal("reopen refunded child occupancy or executed a request")
	}
	before, err := s.rt.opts.Store.Load(t.Context(), opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, commit := range before.Commits {
		for _, r := range commit.ControlRecords {
			if r.Type == "invocation_budget" {
				count++
			}
		}
	}
	if count != 6 {
		t.Fatalf("expected logical and physical reservations for both children, got %d", count)
	}
}

// Compile-time use of the existing store seam documents that this test does
// not require a test-only production store hook.
var _ store.Store = (*p3ChildAttemptFaultStore)(nil)
