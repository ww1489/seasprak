package sessions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/testkit"
)

type gatedOutputProcess struct {
	entered chan agent.ProgressSink
	release chan struct{}
}

func (p *gatedOutputProcess) Execute(ctx context.Context, req agent.AuthorizedProcess, progress agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	if progress != nil {
		if err := progress.WriteProgress(ctx, agent.ProcessProgress{Stream: "stdout", Text: "真实输出 中文🚀", ContentRef: "opaque-secret-ref", Sequence: 999}); err != nil {
			return agent.ProcessObservation{Started: true, SideEffect: "unknown"}, err
		}
	}
	p.entered <- progress
	select {
	case <-p.release:
		return agent.ProcessObservation{Started: true, Terminated: true, Content: "final result", SideEffect: "none"}, nil
	case <-ctx.Done():
		return agent.ProcessObservation{Started: true, SideEffect: "unknown"}, ctx.Err()
	}
}
func (*gatedOutputProcess) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

func TestProcessOutputHasSinkBeforeBackendCompletes(t *testing.T) {
	backend := &gatedOutputProcess{entered: make(chan agent.ProgressSink, 1), release: make(chan struct{})}
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "done"})
	def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "process-operations", Effect: "read", Argv: []string{"fixture"}}}
	s, err := CreateAgentSession(t.Context(), Options{SessionID: "output-gated", Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{def}, Operations: tools.Operations{Process: backend}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	released := false
	defer func() {
		if !released {
			close(backend.release)
		}
	}()
	sub := s.SubscribeEvents(config.DefaultLimits())
	defer sub.Close()
	receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"work"}`)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case sink := <-backend.entered:
		if sink == nil {
			t.Fatal("claimed process received nil progress sink; actual output cannot reach subscribers before completion")
		}
		view, err := s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range view.Calls {
			if !call.Claimed || call.Observation != nil {
				t.Fatalf("before backend exit: call=%+v", call)
			}
		}
		if model.Calls() != 1 || view.Traces[receipt.TraceID].State != "running" {
			t.Fatalf("backend completed early: model calls=%d trace=%+v", model.Calls(), view.Traces[receipt.TraceID])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("process did not start")
	}
	var delta agent.ToolOutputDelta
	var event agent.Event
	deadline := time.After(10 * time.Second)
	for event.Type != "tool.output.delta" {
		select {
		case got, ok := <-sub.Events:
			if !ok {
				t.Fatalf("subscriber closed before output: %v", sub.Err())
			}
			event = got
		case <-deadline:
			t.Fatal("no actual tool output before backend completion")
		}
	}
	if err := json.Unmarshal(event.Payload, &delta); err != nil {
		t.Fatal(err)
	}
	if delta.Stream != "stdout" || delta.Text != "真实输出 中文🚀" || event.StreamID == "" || event.ChunkSeq == nil || *event.ChunkSeq != 1 || event.EventID != "" || event.DurableSeq != nil || strings.Contains(string(event.Payload), "opaque-secret-ref") {
		t.Fatalf("invalid temporary output: %+v payload=%s", event, event.Payload)
	}
	for _, call := range s.rt.manager.View().Calls {
		if delta.ToolCallID != call.Call.CallID || delta.ToolCallID == call.Call.ProviderCallID {
			t.Fatalf("output call identity=%q product=%q provider=%q", delta.ToolCallID, call.Call.CallID, call.Call.ProviderCallID)
		}
	}
	before, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range before.Calls {
		if call.Observation != nil {
			t.Fatalf("observation preceded backend completion: %+v", call)
		}
	}
	if model.Calls() != 1 {
		t.Fatalf("next model call preceded backend completion: %d", model.Calls())
	}
	close(backend.release)
	released = true
	waitSessionTrace(t, s, receipt.TraceID, "completed")
	if model.Calls() != 2 {
		t.Fatalf("model calls=%d", model.Calls())
	}
	for _, call := range s.rt.manager.View().Calls {
		if call.Observation == nil || call.Observation.Content != "final result" {
			t.Fatalf("final result was not saved: %+v", call)
		}
	}
}
