package sessions

import (
	"context"
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2SelectionSearchToolsNextTurn(t *testing.T) {
	first, last := make(chan struct{}), make(chan struct{})
	m := &hiddenSelectionModel{FakeModel: testkit.NewFake(
		testkit.Step{Gate: first, ToolCalls: []schema.FunctionToolCall{{CallID: "bootstrap", Name: "unknown", Arguments: `{}`}}},
		testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "search", Name: "search_tools", Arguments: `{"query":"select:hidden"}`}, {CallID: "early", Name: "hidden", Arguments: `{}`}}},
		testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "actual", Name: "hidden", Arguments: `{}`}}},
		testkit.Step{Gate: last, Text: "done"}), requests: make(chan hiddenSelectionRequest, 8)}
	var runs atomic.Int32
	defs := []tools.Definition{{Name: "hidden", Version: "1", Description: "hidden target", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ok", nil }}}
	for _, d := range tools.NewBuiltinDefinitions(tools.BuiltinOptions{}) {
		if d.Name == "search_tools" {
			defs = append(defs, d)
		}
	}
	s, err := CreateAgentSession(t.Context(), Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: m, Tools: defs, ResourceScheduler: tools.NewResourceScheduler()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	receipt := submitOutput(t, s)
	hiddenSelectionNextRequest(t, m)
	_, err = s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: receipt.TraceID, ToolNames: []string{"search_tools"}, ExpectedRevision: s.rt.manager.View().LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	close(first)
	req := hiddenSelectionNextRequest(t, m)
	if !slices.Equal(req.tools, []string{"search_tools"}) {
		t.Fatalf("search turn inventory %v", req.tools)
	}
	req = hiddenSelectionNextRequest(t, m)
	if !slices.Contains(req.tools, "hidden") {
		t.Fatalf("next turn inventory %v", req.tools)
	}
	hiddenSelectionNextRequest(t, m)
	v := s.rt.manager.View()
	for _, call := range v.Calls {
		if call.Call.ProviderCallID == "early" && (call.Claimed || call.Observation == nil || call.Observation.Status != "denied") {
			t.Fatalf("early call executed: %+v", call)
		}
	}
	if runs.Load() != 1 || v.Traces[receipt.TraceID].Usage.ToolExecutions != 2 || m.Calls() != 4 {
		t.Fatalf("runs=%d budget=%d model=%d", runs.Load(), v.Traces[receipt.TraceID].Usage.ToolExecutions, m.Calls())
	}
	// Re-reading the accepted search call must reuse its observation, without
	// accepting another pending selection or claiming another tool execution.
	frame := activityFrame(t, s)
	executor, err := tools.NewExecutor(frame.scope.Generation, s.rt.searchToolDefinitions(), s.rt, sessionAuthorizer{rt: s.rt, scope: frame.scope}, frame.budget)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range v.Calls {
		if call.Call.ProviderCallID != "search" {
			continue
		}
		var result struct {
			Candidates []ToolCandidate
			ApplyAt    string
		}
		if err := json.Unmarshal([]byte(call.Observation.Content), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Candidates) != 1 || result.Candidates[0].Generation != frame.scope.Generation || result.Candidates[0].Name != "hidden" || result.ApplyAt != "next_turn" {
			t.Fatalf("candidate result %+v", result)
		}
		out, err := executor.Run(t.Context(), call.Scope, call.Call.ProviderCallID, call.Call.Name, call.Call.Arguments)
		if err != nil || out.Status != "succeeded" {
			t.Fatalf("reuse %s %v", out.Status, err)
		}
	}
	after := s.rt.manager.View()
	if len(after.Selections) != len(v.Selections) || after.Traces[receipt.TraceID].Usage.ToolExecutions != 2 || runs.Load() != 1 {
		t.Fatal("search reuse repeated selection or execution")
	}
	close(last)
}
