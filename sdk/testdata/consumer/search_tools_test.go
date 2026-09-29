package consumer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

type consumerToolSearchModel struct {
	calls         atomic.Int32
	entered, gate chan struct{}
}

func (m *consumerToolSearchModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	n := m.calls.Add(1)
	var names []string
	for _, info := range model.GetCommonOptions(nil, opts...).Tools {
		names = append(names, info.Name)
	}
	calls := []schema.FunctionToolCall{}
	switch n {
	case 1:
		close(m.entered)
		select {
		case <-m.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		calls = append(calls, schema.FunctionToolCall{CallID: "bootstrap", Name: "unknown", Arguments: `{}`})
	case 2:
		if !slices.Equal(names, []string{"search_tools"}) {
			return nil, fmt.Errorf("search inventory %v", names)
		}
		calls = append(calls, schema.FunctionToolCall{CallID: "search", Name: "search_tools", Arguments: `{"query":"+hidden target","max_results":1}`}, schema.FunctionToolCall{CallID: "early", Name: "hidden", Arguments: `{}`})
	case 3:
		if !slices.Equal(names, []string{"hidden", "search_tools"}) {
			return nil, fmt.Errorf("selected inventory %v", names)
		}
		raw, err := consumerDiscoveryResult(in, "search")
		if err != nil {
			return nil, err
		}
		var result struct {
			Candidates []sdk.ToolCandidate
			ApplyAt    string
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return nil, err
		}
		if result.ApplyAt != "next_turn" || len(result.Candidates) != 1 || result.Candidates[0].Name != "hidden" || result.Candidates[0].Generation == "" {
			return nil, fmt.Errorf("invalid candidate result")
		}
		calls = append(calls, schema.FunctionToolCall{CallID: "actual", Name: "hidden", Arguments: `{}`})
	case 4:
		return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "done"})}, Extra: map[string]any{"seasprak.finish": "stop"}}, nil
	default:
		return nil, fmt.Errorf("unexpected repeat")
	}
	out := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, Extra: map[string]any{"seasprak.finish": "tool_calls"}}
	for i := range calls {
		out.ContentBlocks = append(out.ContentBlocks, schema.NewContentBlock(&calls[i]))
	}
	return out, nil
}
func (m *consumerToolSearchModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	out, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{out}), nil
}
func TestSDKConsumerSearchToolsNextTurn(t *testing.T) {
	m := &consumerToolSearchModel{entered: make(chan struct{}), gate: make(chan struct{})}
	var runs atomic.Int32
	defs := []sdk.ToolDefinition{{Name: "hidden", Version: "1", Description: "hidden target", Schema: json.RawMessage(`{"type":"object"}`), Execution: sdk.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ok", nil }}}
	for _, d := range sdk.NewBuiltinDefinitions(sdk.BuiltinOptions{}) {
		if d.Name == "search_tools" {
			defs = append(defs, d)
		}
	}
	s, err := sdk.CreateAgentSession(t.Context(), sdk.SessionOptions{SessionID: "consumer-tool-search", Workspace: t.TempDir(), StateRoot: "memory", Profile: sdk.ProfileMemory, Model: m, Tools: defs})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	receipt, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"search tools"}`)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("model did not start")
	}
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetActiveTools(t.Context(), sdk.SetActiveToolsRequest{TraceID: receipt.TraceID, ToolNames: []string{"search_tools"}, ExpectedRevision: snap.Revision}); err != nil {
		t.Fatal(err)
	}
	close(m.gate)
	view := waitConsumerApprovalState(t, s, receipt.TraceID, "completed")
	if runs.Load() != 1 || m.calls.Load() != 4 || view.Traces[receipt.TraceID].Usage.ToolExecutions != 2 {
		t.Fatalf("runs=%d model=%d usage=%+v", runs.Load(), m.calls.Load(), view.Traces[receipt.TraceID].Usage)
	}
	found := false
	for _, call := range view.Calls {
		if call.Call.ProviderCallID == "early" {
			found = true
			if call.Claimed || call.Observation == nil || call.Observation.Status != "denied" || call.Observation.Executed {
				t.Fatalf("early call %+v", call)
			}
		}
	}
	if !found {
		t.Fatal("missing early call")
	}
}
