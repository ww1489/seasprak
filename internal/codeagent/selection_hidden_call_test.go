package codeagent

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
	"github.com/ww1489/seasprak/internal/codeagent/state"
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
	hiddenSelectionAcceptedModelCall(t, false)
}

func TestSelectionHiddenObservationReuseAcrossActivityRenewal(t *testing.T) {
	hiddenSelectionAcceptedModelCall(t, true)
}

func hiddenSelectionAcceptedModelCall(t *testing.T, renewActivity bool) {
	t.Helper()
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
				// Reuse asserts a global commit sequence. Keep unrelated activity
				// renewals deterministic while the final model request is blocked.
				clock := newManualActivityClock()
				if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.clock = clock; return nil }); err != nil {
					t.Fatal(err)
				}
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
				if renewActivity {
					view = hiddenSelectionRenewActivity(t, s, clock, view, receipt.TraceID)
				}
				before := view.LastSeq
				out, err := executor.Run(t.Context(), call.Scope, call.Call.ProviderCallID, call.Call.Name, call.Call.Arguments)
				after := s.rt.manager.View()
				if err != nil || out.Status != "denied" || out.Executed || after.LastSeq != before || runs.Load() != 0 {
					t.Fatalf("saved observation was not reused safely: status=%s executed=%v errorPresent=%v seq=%d->%d backend=%d", out.Status, out.Executed, err != nil, before, after.LastSeq, runs.Load())
				}
				if !reflect.DeepEqual(view, after) {
					t.Fatal("saved observation reuse changed durable state")
				}
				close(finalGate)
				activityWait(t, frame)
				waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[receipt.TraceID].State) })
				if s.rt.manager.View().Traces[receipt.TraceID].State != "completed" {
					t.Fatal("denied call prevented the model trace from completing")
				}
				// The retained controlled RunDirect path has no model visibility
				// requirement, but still performs authorization, claim and budgeting.
				directCall := runControlledDirect(t, def)
				view = s.rt.manager.View()
				if runs.Load() != 1 || model.Calls() != 3 || !reflect.DeepEqual(hiddenSelectionCall(t, view), call) {
					t.Fatal("controlled direct call was blocked or changed the denied model call")
				}
				if directCall.Scope.TurnID != "" || !directCall.Claimed || directCall.Observation == nil || !directCall.Observation.Executed || directCall.Observation.Status != "succeeded" {
					t.Fatalf("direct call lost its own execution identity: %+v", directCall)
				}
			})
		}
	}
}

// A renewal legitimately advances LastSeq without touching calls, observations,
// claims, identities or request/tool usage. Finish that independent commit before
// taking the reuse baseline; reuse itself must still leave the entire View equal.
func hiddenSelectionRenewActivity(t *testing.T, s *AgentSession, clock *manualActivityClock, before state.View, traceID string) state.View {
	t.Helper()
	clock.advance(500 * time.Millisecond)
	waitResumeCondition(t, func() bool {
		return s.rt.manager.View().Traces[traceID].Activity.Revision > before.Traces[traceID].Activity.Revision
	})
	// ReserveActivity publishes its state before the runtime installs the lease.
	if err := s.rt.do(t.Context(), func(*runtime) error { return nil }); err != nil {
		t.Fatal(err)
	}
	after := s.rt.manager.View()
	stored, err := s.rt.opts.Store.Load(t.Context(), s.rt.opts.SessionID)
	if err != nil || len(stored.Commits) == 0 {
		t.Fatal("could not read activity renewal commit")
	}
	commit := stored.Commits[len(stored.Commits)-1]
	if stored.LastSeq != before.LastSeq+1 || commit.CommitSeq != before.LastSeq+1 || len(commit.ControlRecords) != 1 || commit.ControlRecords[0].Type != "trace" || commit.ControlRecords[0].ID != traceID || len(commit.Entries) != 0 || len(commit.Events) != 0 || len(commit.BranchUpdates) != 0 {
		t.Fatal("activity renewal did not append exactly one trace-only commit")
	}
	// These are private View copies, not the manager's live state. Specify the
	// complete expected delta, so all execution and budget facts remain checked.
	before.LastSeq++
	before.Traces[traceID].Activity.Revision++
	before.Traces[traceID].Activity.Settled += 500 * time.Millisecond
	if !reflect.DeepEqual(before, after) {
		t.Fatal("activity renewal changed state beyond its revision and settled time")
	}
	return after
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
