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
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func nextToolDelta(t *testing.T, sub *Subscription) (agent.Event, agent.ToolOutputDelta) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event, ok := <-sub.Events:
			if !ok {
				t.Fatalf("subscription ended: %v", sub.Err())
			}
			if event.Type == "tool.output.delta" {
				var delta agent.ToolOutputDelta
				if err := json.Unmarshal(event.Payload, &delta); err != nil {
					t.Fatal(err)
				}
				return event, delta
			}
		case <-deadline:
			t.Fatal("tool output not delivered")
		}
	}
}

func TestSessionSyncToolsDeliverContentWithoutChangingModelResult(t *testing.T) {
	const arguments = `{"n":9007199254740993,"text":"中文"}`
	for _, kind := range []string{"invokable", "enhanced-invokable"} {
		t.Run(kind, func(t *testing.T) {
			gate := make(chan struct{})
			entered := make(chan struct{}, 1)
			defer func() {
				select {
				case <-gate:
				default:
					close(gate)
				}
			}()
			model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "original-中文", Name: "work", Arguments: arguments}}}, testkit.Step{Text: "done"})
			var runs int
			def := tools.Definition{Name: "work", Version: "1", ToolInterface: kind, Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"text":{"type":"string"}},"required":["n","text"]}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "read"}, RunWithOutput: func(ctx context.Context, args json.RawMessage, output agent.ToolOutputSink) (string, error) {
				runs++
				if string(args) != arguments {
					t.Errorf("arguments lost original bytes: %s", args)
				}
				if err := output.WriteOutput(ctx, agent.ToolOutputChunk{Text: "only subscriber sees 中文🚀"}); err != nil {
					return "", err
				}
				entered <- struct{}{}
				select {
				case <-gate:
					return "final-only", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}}
			s, err := CreateAgentSession(t.Context(), Options{SessionID: "output-" + kind, Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{def}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			sub := s.SubscribeEvents(config.DefaultLimits())
			defer sub.Close()
			receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"work"}`)})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatalf("backend not entered; model=%d trace=%+v calls=%+v", model.Calls(), s.rt.manager.View().Traces[receipt.TraceID], s.rt.manager.View().Calls)
			}
			ev, delta := nextToolDelta(t, sub)
			if delta.Text != "only subscriber sees 中文🚀" || delta.Stream != "output" || ev.ChunkSeq == nil || *ev.ChunkSeq != 1 {
				t.Fatalf("event=%+v delta=%+v", ev, delta)
			}
			before, err := s.Snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if runs != 1 || model.Calls() != 1 || len(before.Calls) != 1 || before.Traces[receipt.TraceID].Usage.ToolExecutions != 1 {
				t.Fatalf("before completion runs=%d model=%d calls=%d", runs, model.Calls(), len(before.Calls))
			}
			for _, call := range before.Calls {
				if !call.Claimed || call.Observation != nil || delta.ToolCallID != call.Call.CallID || delta.ToolCallID == call.Call.ProviderCallID {
					t.Fatalf("premature observation or wrong identity: %+v output=%+v", call, delta)
				}
			}
			close(gate)
			waitSessionTrace(t, s, receipt.TraceID, "completed")
			after := s.rt.manager.View()
			if runs != 1 || model.Calls() != 2 {
				t.Fatalf("runs=%d model=%d", runs, model.Calls())
			}
			for _, call := range after.Calls {
				if call.Observation == nil || call.Observation.Content != "final-only" {
					t.Fatalf("saved result=%+v", call)
				}
			}
			raw, err := json.Marshal(after.Messages)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "only subscriber sees") {
				t.Fatal("temporary output entered model history")
			}
		})
	}
}

func TestToolCallbackModeIsPartOfReopenGeneration(t *testing.T) {
	for _, initialOutput := range []bool{false, true} {
		name := "run-to-output"
		if initialOutput {
			name = "output-to-run"
		}
		t.Run(name, func(t *testing.T) {
			def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`)}
			plain := func(context.Context, json.RawMessage) (string, error) { return "old", nil }
			withOutput := func(context.Context, json.RawMessage, agent.ToolOutputSink) (string, error) { return "new", nil }
			if initialOutput {
				def.RunWithOutput = withOutput
			} else {
				def.Run = plain
			}
			opts := Options{SessionID: "output-mode-reopen", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: testkit.NewFake(), Tools: []tools.Definition{def}}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			original := s.rt.generation
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			old, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := old.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if initialOutput {
				opts.Tools[0].RunWithOutput = nil
				opts.Tools[0].Run = plain
			} else {
				opts.Tools[0].Run = nil
				opts.Tools[0].RunWithOutput = withOutput
			}
			opened, err := OpenAgentSession(t.Context(), opts)
			if opened != nil {
				_ = opened.Close(context.Background())
			}
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
				t.Fatalf("same generation %s replaced callback mode: %v", original, err)
			}
		})
	}
}

func TestSessionRejectsConflictingToolCallbacks(t *testing.T) {
	def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { return "", nil }, RunWithOutput: func(context.Context, json.RawMessage, agent.ToolOutputSink) (string, error) { return "", nil }}
	s, err := CreateAgentSession(t.Context(), Options{SessionID: "callbacks", Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: testkit.NewFake(), Tools: []tools.Definition{def}})
	if s != nil {
		_ = s.Close(context.Background())
	}
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
		t.Fatalf("ambiguous callbacks: %v", err)
	}
}
