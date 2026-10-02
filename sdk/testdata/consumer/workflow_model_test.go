package consumer_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

type consumerWorkflowModel struct {
	calls   atomic.Int32
	invalid string
}

func (m *consumerWorkflowModel) Generate(ctx context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
	m.calls.Add(1)
	msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "accepted"})}, Extra: map[string]any{"seasprak.finish": "stop"}}
	switch m.invalid {
	case "truncated":
		msg.Extra["seasprak.finish"] = "length"
	case "missing_finish":
		msg.Extra = nil
	case "wrong_role":
		msg.Role = schema.AgenticRoleTypeUser
	case "unexpected_tool":
		msg.ContentBlocks = append(msg.ContentBlocks, schema.NewContentBlock(&schema.FunctionToolCall{CallID: "unrequested", Name: "echo", Arguments: `{}`}))
		msg.Extra["seasprak.finish"] = "tool_calls"
	}
	return msg, ctx.Err()
}
func (m *consumerWorkflowModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}
func TestWorkflowConsumerModelValidationControlsRealDownstreamTool(t *testing.T) {
	for _, invalid := range []string{"", "truncated", "missing_finish", "unexpected_tool", "wrong_role"} {
		t.Run(map[string]string{"": "complete", "truncated": "truncated", "missing_finish": "missing_finish", "unexpected_tool": "unexpected_tool", "wrong_role": "wrong_role"}[invalid], func(t *testing.T) {
			m := &consumerWorkflowModel{invalid: invalid}
			var tools atomic.Int32
			def := constantWorkflow()
			def.Nodes = []sdk.WorkflowNode{{ID: "s", Type: "start"}, {ID: "m", Type: "model", Model: "chosen", Prompt: "process"}, {ID: "t", Type: "tool", Tool: "echo", Inputs: map[string]sdk.WorkflowValue{"q": {Ref: &sdk.WorkflowRef{Node: "m", Field: "text"}}}}, {ID: "e", Type: "end", Inputs: map[string]sdk.WorkflowValue{"result": {Ref: &sdk.WorkflowRef{Node: "t", Field: "result"}}}}}
			def.Edges = []sdk.WorkflowEdge{{From: "s", To: "m"}, {From: "m", To: "t"}, {From: "t", To: "e"}}
			w, err := sdk.CreateWorkflowAgent(t.Context(), sdk.WorkflowOptions{Workspace: t.TempDir(), StateRoot: t.TempDir(), Definition: def, Models: map[string]model.AgenticModel{"chosen": m}, Tools: []sdk.ToolDefinition{{Name: "echo", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: sdk.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) { tools.Add(1); return "tool-result", nil }}}, Principal: "local", GenerationFingerprint: "consumer-v1"})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close(context.Background())
			_, err = w.SubmitInput(t.Context(), sdk.WorkflowInputCommand{Input: json.RawMessage(`{"name":"a"}`), Principal: "local"})
			if err != nil {
				t.Fatal(err)
			}
			s := waitConsumerWorkflow(t, w)
			want := int32(1)
			if invalid != "" {
				want = 0
				if s.State != "failed" || s.ErrorCode != sdk.CodeInvalidArgument {
					t.Errorf("invalid response run=%s code=%s", s.State, s.ErrorCode)
				}
			} else if s.State != "completed" {
				t.Fatalf("valid run %+v", s)
			}
			if m.calls.Load() != 1 || tools.Load() != want || s.Usage.ToolExecutions != int(want) || s.Usage.LogicalModelCalls != 1 || s.Usage.TransportRequests != 1 {
				t.Fatalf("actual calls model=%d tools=%d usage=%+v", m.calls.Load(), tools.Load(), s.Usage)
			}
		})
	}
}
