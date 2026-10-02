package codeagent

import (
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP3ChildAttemptCancelHasUniquePrivateTerminal(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	var effects atomic.Int32
	child := newP3ChildAttemptModel(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "late-child-tool", Name: "probe", Arguments: `{}`}}})
	main := testkit.NewFake(delegateCall("worker", "own task"), testkit.Step{Text: "parent must not continue"})
	opts := subagentOptions(agentRoots(t), "child-attempt-cancel", main,
		[]agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"probe"}}},
		[]tools.Definition{countedTool("probe", &effects, nil)})
	s := openSubagentSession(t, opts, true)
	child.manager = s.rt.manager
	in := submitPrompt(t, s, "delegate then cancel")
	waitFor(t, func() bool { return child.FakeModel.Calls() == 1 })
	if err := s.Cancel(t.Context(), in.TraceID); err != nil {
		t.Fatal(err)
	}
	v := s.rt.manager.View()
	inv := onlyInvocation(t, v)
	attempts := childAttemptRecords(v, inv.ID)
	if len(attempts) != 1 {
		t.Fatal("cancelled child lost its original started attempt")
	}
	initial := attempts[0]
	result, ended := v.AttemptResults[initial.ID]
	if !ended || result.State != "aborted" || result.ExpectedRevision != 1 || result.Revision != 2 {
		t.Fatalf("cancelled child must persist one aborted terminal, got ended=%t state=%q", ended, result.State)
	}
	details := v.AttemptDetails[result.DiagnosticRef]
	if details.AttemptID != initial.ID || len(details.Usage) != 2 || result.UsageRef != result.DiagnosticRef {
		t.Fatal("cancelled child lost committed physical request evidence")
	}
	stored, err := s.rt.opts.Store.Load(t.Context(), opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, commit := range stored.Commits {
		for _, r := range commit.ControlRecords {
			if r.Type != "model_attempt_transition" {
				continue
			}
			var terminal state.ModelAttemptTransition
			if json.Unmarshal(r.Payload, &terminal) != nil {
				t.Fatal("invalid terminal record")
			}
			if terminal.AttemptID == initial.ID {
				terminals++
				if len(commit.Entries) != 0 {
					t.Fatal("cancelled child wrote parent history")
				}
			}
		}
	}
	if terminals != 1 || child.generates.Load() != 1 || child.physical.Load() != 2 || effects.Load() != 0 || main.Calls() != 1 || len(inv.CallIDs) != 0 || len(inv.MessageIDs) != 0 || v.Traces[in.TraceID].State != "cancelled" || !v.Traces[in.TraceID].Settled {
		t.Fatal("cancellation repeated a terminal, accepted a late response, or continued parent work")
	}
	for _, msg := range v.Messages {
		if msg.Scope.InvocationID == inv.ID {
			t.Fatal("cancelled child candidate entered parent history")
		}
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s = openSubagentSession(t, opts, false)
	after := s.rt.manager.View()
	if !reflect.DeepEqual(v.AttemptResults, after.AttemptResults) || !reflect.DeepEqual(v.AttemptDetails, after.AttemptDetails) || after.Traces[in.TraceID].Usage != v.Traces[in.TraceID].Usage || child.physical.Load() != 2 || main.Calls() != 1 {
		t.Fatal("reopen changed cancelled attempts, refunded reservations, or invoked execution")
	}
}
