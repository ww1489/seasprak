package sessions

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"

	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestEnhancedSessionPreservesArgumentsAndOneClaim(t *testing.T) {
	const arguments = `{"n":9007199254740993,"text":"中文"}`
	for _, kind := range []string{"invokable", "enhanced-invokable"} {
		t.Run(kind, func(t *testing.T) {
			var runs atomic.Int32
			definition := tools.Definition{Name: "work", Version: "1", ToolInterface: kind, Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"text":{"type":"string"}},"required":["n","text"]}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "read"}, Run: func(_ context.Context, args json.RawMessage) (string, error) {
				if string(args) != arguments {
					t.Errorf("arguments=%q", args)
				}
				runs.Add(1)
				return "完成", nil
			}}
			model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "original-中文", Name: "work", Arguments: arguments}}}, testkit.Step{Text: "done"})
			s, err := CreateAgentSession(t.Context(), Options{SessionID: "enhanced-session", Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{definition}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitSessionTrace(t, s, receipt.TraceID, "completed")
			view := s.rt.manager.View()
			if runs.Load() != 1 || view.Traces[receipt.TraceID].Usage.ToolExecutions != 1 || len(view.Calls) != 1 {
				t.Fatalf("runs=%d usage=%+v calls=%d", runs.Load(), view.Traces[receipt.TraceID].Usage, len(view.Calls))
			}
			for _, call := range view.Calls {
				if call.Call.ProviderCallID != "original-中文" || call.Call.Arguments != arguments || !call.Claimed || call.Observation == nil || call.Observation.Content != "完成" || call.Observation.Status != "succeeded" {
					t.Fatalf("call=%+v", call)
				}
			}
		})
	}
}

func TestEnhancedSessionUnavailableApprovalFailsWithoutClaim(t *testing.T) {
	var runs atomic.Int32
	definition := tools.Definition{Name: "work", Version: "1", ToolInterface: "enhanced-invokable", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "read", RequestedGrantRef: "request"}, Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		return "unexpected", nil
	}}
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "original", Name: "work", Arguments: `{}`}}})
	s, err := CreateAgentSession(t.Context(), Options{SessionID: "enhanced-ask", Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{definition}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"work"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitSessionTrace(t, s, receipt.TraceID, "failed")
	view := s.rt.manager.View()
	if runs.Load() != 0 || view.Traces[receipt.TraceID].Usage.ToolExecutions != 0 || !strings.Contains(view.Traces[receipt.TraceID].Error, product.CodeResourceUnavailable) || len(view.Calls) != 1 {
		t.Fatalf("runs=%d trace=%+v calls=%d", runs.Load(), view.Traces[receipt.TraceID], len(view.Calls))
	}
	for _, call := range view.Calls {
		if call.Claimed || call.Observation == nil || call.Observation.Status != "denied" || call.Observation.Executed {
			t.Fatalf("unsafe approval result: %+v", call)
		}
	}
}

func TestReopenPreservesDefaultInterfaceSpelling(t *testing.T) {
	def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`)}
	opts := Options{SessionID: "default-interface", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: testkit.NewFake(), Tools: []tools.Definition{def}}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	opts.Tools[0].ToolInterface = "invokable"
	reopened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatalf("old invokable generation rejected equivalent explicit spelling: %v", err)
	}
	if err := reopened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReopenRejectsChangedToolInterface(t *testing.T) {
	var def tools.Definition
	if err := json.Unmarshal([]byte(`{"Name":"work","Version":"1","Schema":{"type":"object"},"ToolInterface":"enhanced-invokable"}`), &def); err != nil {
		t.Fatal(err)
	}
	opts := Options{SessionID: "interface-generation", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: testkit.NewFake(), Tools: []tools.Definition{def}}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenAgentSession(t.Context(), opts); err != nil {
		t.Fatal(err)
	} else if err := reopened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var changed tools.Definition
	if err := json.Unmarshal([]byte(`{"Name":"work","Version":"1","Schema":{"type":"object"},"ToolInterface":"invokable"}`), &changed); err != nil {
		t.Fatal(err)
	}
	opts.Tools = []tools.Definition{changed}
	reopened, err := OpenAgentSession(t.Context(), opts)
	if reopened != nil {
		_ = reopened.Close(context.Background())
	}
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeIncompatibleVersion {
		t.Fatalf("changed tool interface reopened without generation rejection: %v", err)
	}
}

func TestCreateRejectsUnsettledStreamInterface(t *testing.T) {
	for _, kind := range []string{"streamable", "enhanced-streamable"} {
		t.Run(kind, func(t *testing.T) {
			def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), ToolInterface: kind}
			opts := Options{SessionID: "unsettled-stream", Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: testkit.NewFake(), Tools: []tools.Definition{def}}
			s, err := CreateAgentSession(t.Context(), opts)
			if s != nil {
				_ = s.Close(context.Background())
			}
			pe, ok := product.AsError(err)
			if !ok || pe.Code != product.CodeResourceUnavailable {
				t.Fatalf("stream interface advertised without settled backend lifecycle: %v", err)
			}
		})
	}
}

func TestCreateRejectsUnknownToolInterface(t *testing.T) {
	var def tools.Definition
	if err := json.Unmarshal([]byte(`{"Name":"work","Version":"1","Schema":{"type":"object"},"ToolInterface":"other"}`), &def); err != nil {
		t.Fatal(err)
	}
	opts := Options{SessionID: "unknown-interface", Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: testkit.NewFake(), Tools: []tools.Definition{def}}
	s, err := CreateAgentSession(t.Context(), opts)
	if s != nil {
		_ = s.Close(context.Background())
	}
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeInvalidArgument {
		t.Fatalf("unknown interface accepted: %v", err)
	}
}
