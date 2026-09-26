package sessions

import (
	"context"
	"encoding/json"
	"errors"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func outputSession(t *testing.T, id string, model *testkit.FakeModel, callback func(context.Context, json.RawMessage, agent.ToolOutputSink) (string, error)) *AgentSession {
	t.Helper()
	def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "read"}, RunWithOutput: callback}
	s, err := CreateAgentSession(t.Context(), Options{SessionID: id, Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{def}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}
func submitOutput(t *testing.T, s *AgentSession) agent.InputReceipt {
	t.Helper()
	r, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func outputModel() *testkit.FakeModel {
	return testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "done"})
}

func TestCancelledUncooperativeToolDoesNotStopBeforeReturnOrPublishLateText(t *testing.T) {
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	entered := make(chan agent.ToolOutputSink, 1)
	model := outputModel()
	s := outputSession(t, "cancel-output", model, func(ctx context.Context, _ json.RawMessage, sink agent.ToolOutputSink) (string, error) {
		if err := sink.WriteOutput(ctx, agent.ToolOutputChunk{Text: "before cancel"}); err != nil {
			return "", err
		}
		entered <- sink
		<-release // deliberately ignores context cancellation
		return "backend exited", nil
	})
	sub := s.SubscribeEvents(config.DefaultLimits())
	defer sub.Close()
	receipt := submitOutput(t, s)
	var sink agent.ToolOutputSink
	select {
	case sink = <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("tool did not start")
	}
	_, delta := nextToolDelta(t, sub)
	if delta.Text != "before cancel" {
		t.Fatalf("initial output=%+v", delta)
	}
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- s.Cancel(context.Background(), receipt.TraceID) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		v, err := s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if v.Traces[receipt.TraceID].State == "cancelling" {
			break
		}
		goruntime.Gosched()
	}
	v, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if v.Traces[receipt.TraceID].State != "cancelling" || v.Traces[receipt.TraceID].Settled || v.Traces[receipt.TraceID].ExecutionStopped || model.Calls() != 1 {
		t.Fatalf("stopped before backend exit: %+v model=%d", v.Traces[receipt.TraceID], model.Calls())
	}
	select {
	case err := <-cancelDone:
		t.Fatalf("Cancel returned before backend exit: %v", err)
	default:
	}
	if err := sink.WriteOutput(context.Background(), agent.ToolOutputChunk{Text: "after cancel"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("background bypassed run cancellation: %v", err)
	}
	close(release)
	select {
	case err := <-cancelDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Cancel did not wait for backend exit")
	}
	v, err = s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if v.Traces[receipt.TraceID].State != "cancelled" || !v.Traces[receipt.TraceID].ExecutionStopped || model.Calls() != 1 {
		t.Fatalf("wrong cancellation outcome: %+v model=%d", v.Traces[receipt.TraceID], model.Calls())
	}
}

func TestSlowOrClosedSubscriberCannotFailTool(t *testing.T) {
	ready := make(chan struct{})
	startWrites := make(chan struct{})
	defer func() {
		select {
		case <-startWrites:
		default:
			close(startWrites)
		}
	}()
	model := outputModel()
	s := outputSession(t, "slow-output", model, func(ctx context.Context, _ json.RawMessage, sink agent.ToolOutputSink) (string, error) {
		close(ready)
		select {
		case <-startWrites:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		for i := 0; i < 3; i++ {
			if err := sink.WriteOutput(ctx, agent.ToolOutputChunk{Text: "real"}); err != nil {
				return "", err
			}
		}
		return "final", nil
	})
	receipt := submitOutput(t, s)
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("tool did not enter")
	}
	slow := s.SubscribeEvents(config.Limits{SubscriptionEvents: 1, SubscriptionBytes: 4096})
	defer slow.Close()
	closed := s.SubscribeEvents(config.DefaultLimits())
	closed.Close()
	close(startWrites)
	waitSessionTrace(t, s, receipt.TraceID, "completed")
	if pe, ok := product.AsError(slow.Err()); !ok || pe.Code != product.CodeResyncRequired {
		t.Fatalf("slow subscription did not isolate overflow: %v", slow.Err())
	}
	view := s.rt.manager.View()
	if model.Calls() != 2 || view.Traces[receipt.TraceID].Usage.ToolExecutions != 1 {
		t.Fatalf("tool affected by subscription: model=%d trace=%+v", model.Calls(), view.Traces[receipt.TraceID])
	}
	for _, call := range view.Calls {
		if call.Observation == nil || call.Observation.Content != "final" {
			t.Fatalf("subscriber changed result: %+v", call)
		}
	}
	for _, ev := range view.Events {
		if ev.Type == "tool.output.delta" {
			t.Fatal("temporary output persisted")
		}
	}
}

func TestTerminalAndOldExecutionRejectLateOutputFact(t *testing.T) {
	var oldScope agent.ExecutionScope
	var oldFact agent.Fact
	ready := make(chan struct{})
	gate := make(chan struct{})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	secondModelGate := make(chan struct{})
	defer func() {
		select {
		case <-secondModelGate:
		default:
			close(secondModelGate)
		}
	}()
	model := testkit.NewFake(
		testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}},
		testkit.Step{Text: "done"},
		testkit.Step{Text: "second done", Gate: secondModelGate},
	)
	s := outputSession(t, "late-output", model, func(ctx context.Context, _ json.RawMessage, sink agent.ToolOutputSink) (string, error) {
		if err := sink.WriteOutput(ctx, agent.ToolOutputChunk{Text: "first"}); err != nil {
			return "", err
		}
		close(ready)
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "final", nil
	})
	sub := s.SubscribeEvents(config.DefaultLimits())
	defer sub.Close()
	receipt := submitOutput(t, s)
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("tool not entered")
	}
	ev, _ := nextToolDelta(t, sub)
	view := s.rt.manager.View()
	for _, call := range view.Calls {
		oldScope = call.Scope
		oldFact = agent.Fact{Kind: "tool_output"}
		payload, _ := json.Marshal(agent.ToolOutputFact{ToolOutputDelta: agent.ToolOutputDelta{ToolCallID: call.Call.CallID, Stream: "output", Text: "late"}, CallID: call.Call.CallID, StreamID: ev.StreamID, ChunkSeq: 2})
		oldFact.Payload = payload
	}
	if oldScope.ExecutionID == "" {
		t.Fatal("missing execution identity")
	}
	var valid agent.ToolOutputFact
	if err := json.Unmarshal(oldFact.Payload, &valid); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*agent.ToolOutputFact){
		func(v *agent.ToolOutputFact) { v.ChunkSeq = 1 },
		func(v *agent.ToolOutputFact) { v.StreamID = "forged" },
		func(v *agent.ToolOutputFact) { v.ToolCallID = "forged" },
		func(v *agent.ToolOutputFact) { v.CallID = "forged" },
		func(v *agent.ToolOutputFact) { v.Stream = "stage" },
	} {
		invalid := valid
		change(&invalid)
		raw, _ := json.Marshal(invalid)
		if pe, ok := product.AsError(s.rt.CommitFact(context.Background(), oldScope, agent.Fact{Kind: "tool_output", Payload: raw})); !ok || pe.Code != product.CodeStateConflict {
			t.Fatalf("stale stream fact accepted: %v", pe)
		}
	}
	close(gate)
	waitSessionTrace(t, s, receipt.TraceID, "completed")
	before := s.rt.manager.View().LastSeq
	if pe, ok := product.AsError(s.rt.CommitFact(context.Background(), oldScope, oldFact)); !ok || pe.Code != product.CodeStateConflict {
		t.Fatalf("terminal output accepted: %v", pe)
	}
	if s.rt.manager.View().LastSeq != before {
		t.Fatal("late output became durable")
	}
	// A new, still-running execution must not inherit the old segment's stream.
	second := submitOutput(t, s)
	current, err := s.rt.call(t.Context(), func(rt *runtime) (any, error) {
		if rt.active == nil {
			return nil, product.NewError(product.CodeStateConflict, "second execution is not active")
		}
		return rt.active.scope, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	newScope := current.(agent.ExecutionScope)
	if newScope.TraceID != second.TraceID || newScope.ExecutionID == oldScope.ExecutionID {
		t.Fatalf("old and current executions are not distinct: old=%+v current=%+v", oldScope, newScope)
	}
	before = s.rt.manager.View().LastSeq
	if pe, ok := product.AsError(s.rt.CommitFact(context.Background(), oldScope, oldFact)); !ok || pe.Code != product.CodeStateConflict {
		t.Fatalf("old execution output accepted while another execution is active: %v", pe)
	}
	if err := s.rt.do(t.Context(), func(rt *runtime) error {
		if rt.active == nil || rt.active.scope.ExecutionID != newScope.ExecutionID || len(rt.active.toolChunks) != 0 {
			return product.NewError(product.CodeStateConflict, "old output changed new execution stream positions")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(secondModelGate)
	waitSessionTrace(t, s, second.TraceID, "completed")
}
