package consumer_test

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

type consumerOutputModel struct {
	calls     atomic.Int32
	finalOnly atomic.Bool
}

func (m *consumerOutputModel) Generate(_ context.Context, input []*schema.AgenticMessage, _ ...einomodel.Option) (*schema.AgenticMessage, error) {
	if m.calls.Add(1) == 1 {
		return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider", Name: "work", Arguments: `{"n":9007199254740993,"text":"中文"}`})}, Extra: map[string]any{"seasprak.finish": "tool_calls"}}, nil
	}
	raw, _ := json.Marshal(input)
	m.finalOnly.Store(strings.Contains(string(raw), "final-only") && !strings.Contains(string(raw), "subscriber-only"))
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "done"})}, Extra: map[string]any{"seasprak.finish": "stop"}}, nil
}
func (m *consumerOutputModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

func TestSDKConsumerReceivesToolContentBeforeCompletion(t *testing.T) {
	model := &consumerOutputModel{}
	gate := make(chan struct{})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	def := sdk.ToolDefinition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"text":{"type":"string"}},"required":["n","text"]}`), RunWithOutput: func(ctx context.Context, args json.RawMessage, output sdk.ToolOutputSink) (string, error) {
		if string(args) != `{"n":9007199254740993,"text":"中文"}` {
			t.Errorf("large integer or Chinese changed: %s", args)
		}
		if err := output.WriteOutput(ctx, sdk.ToolOutputChunk{Text: "subscriber-only 中文🚀"}); err != nil {
			return "", err
		}
		select {
		case <-gate:
			return "final-only", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
	def.Execution.BackendID = "trusted-run"
	def.Execution.Effect = "read"
	s, err := sdk.CreateAgentSession(t.Context(), sdk.SessionOptions{SessionID: "consumer-output", Workspace: t.TempDir(), StateRoot: "memory", Profile: sdk.ProfileMemory, Model: model, Tools: []sdk.ToolDefinition{def}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	sub := s.SubscribeEvents(sdk.DefaultLimits())
	defer sub.Close()
	receipt, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run"}`)})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	var event sdk.Event
	for event.Type != "tool.output.delta" {
		select {
		case got, ok := <-sub.Events:
			if !ok {
				t.Fatalf("subscription ended: %v", sub.Err())
			}
			event = got
		case <-deadline:
			t.Fatal("SDK did not receive tool text before backend completion")
		}
	}
	var delta sdk.ToolOutputDelta
	if err := json.Unmarshal(event.Payload, &delta); err != nil {
		t.Fatal(err)
	}
	if delta.Stream != "output" || delta.Text != "subscriber-only 中文🚀" || event.StreamID == "" || event.ChunkSeq == nil || *event.ChunkSeq != 1 || event.DurableSeq != nil || event.EventID != "" {
		t.Fatalf("unexpected SDK event: %+v payload=%+v", event, delta)
	}
	view, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if model.calls.Load() != 1 || len(view.Calls) != 1 {
		t.Fatalf("model calls=%d tool calls=%d", model.calls.Load(), len(view.Calls))
	}
	for _, call := range view.Calls {
		if !call.Claimed || call.Observation != nil || delta.ToolCallID != call.Call.CallID || delta.ToolCallID == call.Call.ProviderCallID {
			t.Fatalf("tool finished prematurely or wrong product call identity: %+v delta=%+v", call, delta)
		}
	}
	close(gate)
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		view, err = s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if view.Traces[receipt.TraceID].State == "completed" {
			break
		}
		runtime.Gosched()
	}
	if view.Traces[receipt.TraceID].State != "completed" || model.calls.Load() != 2 || !model.finalOnly.Load() {
		t.Fatalf("final trace=%+v calls=%d model received only result=%v", view.Traces[receipt.TraceID], model.calls.Load(), model.finalOnly.Load())
	}
	for _, call := range view.Calls {
		if call.Observation == nil || call.Observation.Content != "final-only" {
			t.Fatalf("observation=%+v", call)
		}
	}
}
