package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/testkit"
)

type hiddenSelectionRequest struct {
	tools   []string
	results []schema.FunctionToolResult
}

// Observe the actual options and paired results delivered to the model, without
// replacing the session's execution sink or its response-acceptance path.
type hiddenSelectionModel struct {
	*testkit.FakeModel
	requests chan hiddenSelectionRequest
}

func (m *hiddenSelectionModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.AgenticMessage, error) {
	request := hiddenSelectionRequest{}
	for _, info := range einomodel.GetCommonOptions(nil, opts...).Tools {
		request.tools = append(request.tools, info.Name)
	}
	for _, message := range input {
		for _, block := range message.ContentBlocks {
			if block != nil && block.FunctionToolResult != nil {
				request.results = append(request.results, *block.FunctionToolResult)
			}
		}
	}
	m.requests <- request
	return m.FakeModel.Generate(ctx, input, opts...)
}

func (m *hiddenSelectionModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	message, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{message}), nil
}

func hiddenSelectionNextRequest(t *testing.T, model *hiddenSelectionModel) hiddenSelectionRequest {
	t.Helper()
	select {
	case request := <-model.requests:
		return request
	case <-time.After(10 * time.Second):
		t.Fatal("model request did not arrive")
		return hiddenSelectionRequest{}
	}
}

func TestSelectionHiddenAcceptedModelCall(t *testing.T) {
	for _, kind := range []string{"invokable", "enhanced-invokable"} {
		for _, enablePending := range []bool{false, true} {
			name := "hidden"
			if enablePending {
				name = "pending-enable"
			}
			t.Run(kind+"/"+name, func(t *testing.T) {
				firstGate, hiddenGate, finalGate := make(chan struct{}), make(chan struct{}), make(chan struct{})
				model := &hiddenSelectionModel{FakeModel: testkit.NewFake(
					// An unknown bootstrap call advances the real loop without spending
					// any tool budget, while selection is submitted through the API.
					testkit.Step{Gate: firstGate, ToolCalls: []schema.FunctionToolCall{{CallID: "bootstrap", Name: "not_registered", Arguments: `{}`}}},
					testkit.Step{Gate: hiddenGate, ToolCalls: []schema.FunctionToolCall{{CallID: "hidden-provider", Name: "hidden", Arguments: `{}`}}},
					testkit.Step{Gate: finalGate, Text: "done"},
				), requests: make(chan hiddenSelectionRequest, 4)}
				var runs atomic.Int32
				def := tools.Definition{Name: "hidden", Version: "1", ToolInterface: kind, Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) {
					runs.Add(1)
					return "actual backend result", nil
				}}
				s, err := CreateAgentSession(t.Context(), Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{def}, ResourceScheduler: tools.NewResourceScheduler()})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close(context.Background())
				receipt := submitOutput(t, s)
				frame := activityFrame(t, s)
				if request := hiddenSelectionNextRequest(t, model); !reflect.DeepEqual(request.tools, []string{"hidden"}) {
					t.Fatalf("generation inventory was not initially visible: %v", request.tools)
				}
				selection, err := s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: receipt.TraceID, ToolNames: []string{}, ExpectedRevision: s.rt.manager.View().LastSeq})
				if err != nil {
					t.Fatal(err)
				}
				close(firstGate)
				if request := hiddenSelectionNextRequest(t, model); len(request.tools) != 0 {
					t.Fatalf("hidden tool was actually sent to model: %v", request.tools)
				}
				if enablePending {
					if _, err := s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: receipt.TraceID, ToolNames: []string{"hidden"}, ExpectedRevision: s.rt.manager.View().LastSeq}); err != nil {
						t.Fatal(err)
					}
				}
				close(hiddenGate)
				request := hiddenSelectionNextRequest(t, model)
				view := s.rt.manager.View()
				call := hiddenSelectionCall(t, view)
				turn := view.Turns[call.Scope.TurnID]
				if len(turn.ToolNames) != 0 || turn.SelectionRevision == 0 || call.Call.SelectionRevision != turn.SelectionRevision || !acceptedAttemptForCall(view, call) {
					t.Fatalf("test did not reach an accepted call bound to the hidden Turn: call=%+v turn=%+v", call, turn)
				}
				active, ok := selectionForOperation(view, selection.OperationID)
				if !ok || active.State != "active" {
					t.Fatalf("selection did not activate: %+v", active)
				}
				if runs.Load() != 0 || call.Claimed || call.Observation == nil || call.Observation.Status != "denied" || call.Observation.SideEffect != "none" || call.Observation.Executed || view.Traces[receipt.TraceID].Usage.ToolExecutions != 0 || len(view.FrozenExecutions) != 0 {
					t.Fatalf("hidden accepted call crossed execution boundary: backend=%d claimed=%v observation=%+v toolBudget=%d frozen=%d", runs.Load(), call.Claimed, call.Observation, view.Traces[receipt.TraceID].Usage.ToolExecutions, len(view.FrozenExecutions))
				}
				hiddenSelectionAssertPair(t, view, call, request)
				if enablePending && !reflect.DeepEqual(request.tools, []string{"hidden"}) {
					t.Fatalf("pending selection did not wait until next Turn: %v", request.tools)
				}
				// A completed observation is reusable even after a later selection.
				// This still uses the real session sink, not a permissive fake.
				executor, err := tools.NewExecutor(call.Call.Generation, []tools.Definition{def}, s.rt, sessionAuthorizer{rt: s.rt, scope: frame.scope}, frame.budget)
				if err != nil {
					t.Fatal(err)
				}
				before := view.LastSeq
				out, err := executor.Run(t.Context(), call.Scope, call.Call.ProviderCallID, call.Call.Name, call.Call.Arguments)
				if err != nil || out.Status != "denied" || out.Executed || s.rt.manager.View().LastSeq != before || runs.Load() != 0 {
					t.Fatalf("saved observation was not reused safely: out=%+v err=%v", out, err)
				}
				close(finalGate)
				activityWait(t, frame)
				waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[receipt.TraceID].State) })
				if s.rt.manager.View().Traces[receipt.TraceID].State != "completed" {
					t.Fatal("denied call prevented the model trace from completing")
				}
				// The same implementation remains available to a trusted direct
				// command, which has no model Turn or visibility requirement.
				direct, err := s.ExecuteCommand(t.Context(), CommandRequest{Name: "hidden", Arguments: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				waitResumeCondition(t, func() bool { return s.rt.manager.View().Operations[direct.OperationID].State == "completed" })
				view = s.rt.manager.View()
				if runs.Load() != 1 || model.Calls() != 3 || !reflect.DeepEqual(hiddenSelectionCall(t, view), call) {
					t.Fatal("direct command was blocked or changed the denied model call")
				}
				directCall := view.Calls[direct.OperationID]
				if directCall.Scope.TurnID != "" || !directCall.Claimed || directCall.Observation == nil || !directCall.Observation.Executed || directCall.Observation.Status != "succeeded" {
					t.Fatalf("direct call lost its own execution identity: %+v", directCall)
				}
			})
		}
	}
}

func hiddenSelectionCall(t *testing.T, view state.View) agent.ToolRecord {
	t.Helper()
	for _, call := range view.Calls {
		if call.Call.ProviderCallID == "hidden-provider" {
			return call
		}
	}
	t.Fatal("hidden provider call was not durably accepted")
	return agent.ToolRecord{}
}

func hiddenSelectionAssertPair(t *testing.T, view state.View, call agent.ToolRecord, request hiddenSelectionRequest) {
	t.Helper()
	if call.Call.CallID == "" || call.Call.CallID == call.Call.ProviderCallID || call.Scope.SessionID == "" || call.Scope.InvocationID == "" || call.Scope.ExecutionID == "" || call.Call.Generation != view.Generation {
		t.Fatalf("product/provider identities are not preserved: %+v", call)
	}
	assistant, result, projected, finished := 0, 0, 0, 0
	for _, message := range view.Messages {
		if message.Scope.TurnID != call.Scope.TurnID || message.Standard == nil {
			continue
		}
		for _, block := range message.Standard.ContentBlocks {
			if block.FunctionToolCall != nil && block.FunctionToolCall.CallID == call.Call.ProviderCallID {
				assistant++
			}
			if block.FunctionToolResult != nil && block.FunctionToolResult.CallID == call.Call.ProviderCallID {
				result++
				if message.Scope.ToolCallID != call.Call.CallID || message.Scope.InvocationID != call.Scope.InvocationID || block.FunctionToolResult.Name != call.Call.Name {
					t.Fatal("result was paired with the wrong product call")
				}
			}
		}
	}
	for _, item := range request.results {
		if item.CallID == call.Call.ProviderCallID && item.Name == call.Call.Name {
			projected++
			if len(item.Content) != 1 || item.Content[0].Text == nil {
				t.Fatal("model received no denial projection")
			}
			var out tools.Outcome
			if err := json.Unmarshal([]byte(item.Content[0].Text.Text), &out); err != nil || out.Status != "denied" || out.SideEffect != "none" || out.Executed || out.Content != call.Observation.Content {
				t.Fatalf("model received a result different from the durable denial: %+v err=%v", out, err)
			}
		}
	}
	for _, event := range view.Events {
		if event.Type == "tool.started" {
			t.Fatal("hidden/unknown calls published a started event")
		}
		if event.Type == "tool.finished" && event.Scope.TurnID == call.Scope.TurnID {
			finished++
		}
	}
	if assistant != 1 || result != 1 || projected != 1 || finished != 1 {
		t.Fatalf("denial pairing: assistant=%d result=%d modelResult=%d finished=%d", assistant, result, projected, finished)
	}
}
