package state_test

import (
	"reflect"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
)

func childAttemptFixture(t *testing.T) (*state.Manager, *failingStore, state.ModelAttempt, agent.AgentMessage, []agent.ToolRecord) {
	t.Helper()
	m, backend := fixture(t)
	input := accept(t, m, "child-attempt", `{"text":"delegate"}`)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.SetTraceState(t.Context(), input.TraceID, "running", false))
	must(m.Consume(t.Context(), input.InputID))
	view := m.View()
	trace := view.Traces[input.TraceID]
	parentScope := agent.ExecutionScope{SessionID: "session", BranchID: view.BranchID, TraceID: trace.ID, InvocationID: trace.InvocationID, ExecutionID: "execution", Generation: "g", TurnID: "parent-turn"}
	must(m.SaveTurn(t.Context(), agent.TurnRecord{ID: parentScope.TurnID, TraceID: trace.ID, InvocationID: trace.InvocationID}))
	parentCall := agent.ToolRecord{Scope: parentScope, Call: agent.FrozenCall{CallID: "delegate", ProviderCallID: "parent-provider", Name: "delegate_task", Arguments: `{"agent":"worker","task":"work"}`, Generation: "g"}}
	parentMessage := agent.AgentMessage{ID: "parent-assistant", Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: "session", TraceID: trace.ID, InvocationID: trace.InvocationID, TurnID: parentScope.TurnID}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: parentCall.Call.ProviderCallID, Name: parentCall.Call.Name, Arguments: parentCall.Call.Arguments})}}}
	must(m.SaveAssistant(t.Context(), parentMessage, []agent.ToolRecord{parentCall}))
	parentCall.Claimed = true
	must(m.SaveCall(t.Context(), parentCall))
	inv := state.Invocation{ID: "child", ParentInvocationID: trace.InvocationID, ParentCallID: parentCall.Call.CallID, TraceID: trace.ID, State: "running", Target: agent.TargetAgent{Name: "worker", Version: "v1", Generation: "g"}}
	must(m.SaveInvocation(t.Context(), inv))
	scope := parentScope
	scope.InvocationID, scope.ParentInvocationID, scope.TurnID = inv.ID, inv.ParentInvocationID, ""
	initial := state.ModelAttempt{ID: "child-attempt", ModelCallID: "child-model-call", MessageID: "child-candidate", StreamID: "child-stream", Scope: scope, State: "started"}
	must(m.SaveRecords(t.Context(), m.View().LastSeq, state.Records{ModelAttempts: []state.ModelAttempt{initial}}))
	msg := agent.AgentMessage{ID: initial.MessageID, Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: scope.SessionID, TraceID: scope.TraceID, InvocationID: scope.InvocationID}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, Extra: map[string]any{"private.fixture": map[string]any{"signature": "child-private-evidence"}}, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "child-provider", Name: "probe", Arguments: `{"child":true}`})}}}
	calls := []agent.ToolRecord{{Scope: scope, Call: agent.FrozenCall{CallID: "child-call", ProviderCallID: "child-provider", Name: "probe", Arguments: `{"child":true}`, Generation: "g"}}}
	return m, backend, initial, msg, calls
}

func requireChildParentUnchanged(t *testing.T, before, after state.View) {
	t.Helper()
	if !reflect.DeepEqual(before.Messages, after.Messages) || !reflect.DeepEqual(before.Nodes, after.Nodes) || !reflect.DeepEqual(before.Offpath, after.Offpath) || !reflect.DeepEqual(before.Branches, after.Branches) || before.LeafID != after.LeafID || !reflect.DeepEqual(before.Turns, after.Turns) {
		t.Fatal("child candidate entered parent history or changed parent turn")
	}
}

func TestP3ChildAttemptAcceptedWithoutParentTurn(t *testing.T) {
	m, _, initial, msg, calls := childAttemptFixture(t)
	before := m.View()
	if err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "accepted", "tool_calls", &msg, calls); err != nil {
		t.Fatalf("child acceptance must not require a parent Turn: %v", err)
	}
	after := m.View()
	requireChildParentUnchanged(t, before, after)
	if after.ModelAttempts[initial.ID] != initial || after.AttemptResults[initial.ID].State != "accepted" || len(after.AttemptResults) != 1 || len(after.Calls) != len(before.Calls)+1 {
		t.Fatal("child acceptance lost the immutable attempt or atomic calls")
	}
}

func TestP3ChildAuxiliaryAttemptCannotAdmitTools(t *testing.T) {
	for _, purpose := range []string{"compaction"} {
		t.Run(purpose, func(t *testing.T) {
			m, _, initial, msg, calls := childAttemptFixture(t)
			initial.ID, initial.ModelCallID, initial.MessageID, initial.StreamID, initial.Purpose = "aux-attempt", "aux-call", "aux-candidate", "aux-stream", purpose
			if err := m.SaveRecords(t.Context(), m.View().LastSeq, state.Records{ModelAttempts: []state.ModelAttempt{initial}}); err != nil {
				t.Fatal(err)
			}
			msg.ID = initial.MessageID
			before := m.View()
			err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "accepted", "tool_calls", &msg, calls)
			requireP2Code(t, err, product.CodeInvalidArgument)
			if !reflect.DeepEqual(before, m.View()) || m.Fault() != nil {
				t.Fatal("auxiliary model admitted a tool or published a terminal on rejection")
			}
		})
	}
}

func TestP3ChildAttemptFailedCandidateStaysPrivate(t *testing.T) {
	m, _, initial, msg, _ := childAttemptFixture(t)
	msg.Status = agent.StatusIncomplete
	msg.Standard.ContentBlocks = nil
	before := m.View()
	if err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "failed", "", &msg, nil); err != nil {
		t.Fatal(err)
	}
	requireChildParentUnchanged(t, before, m.View())
}
