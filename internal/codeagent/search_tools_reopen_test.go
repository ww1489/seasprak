package codeagent

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

type searchReopenModel struct{ *hiddenSelectionModel }

func (*searchReopenModel) Configuration() llm.ModelConfig {
	return selectionTrustedConfig("search-model")
}

func TestP2SelectionSearchToolsPendingDiskReopen(t *testing.T) {
	first := make(chan struct{})
	m := &searchReopenModel{&hiddenSelectionModel{FakeModel: testkit.NewFake(testkit.Step{Gate: first, ToolCalls: []schema.FunctionToolCall{{CallID: "bootstrap", Name: "unknown", Arguments: `{}`}}}, testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "search", Name: "search_tools", Arguments: `{"query":"select:hidden"}`}, {CallID: "approval", Name: "wait", Arguments: `{}`}}}), requests: make(chan hiddenSelectionRequest, 8)}}
	var runs, approved atomic.Int32
	defs := []tools.Definition{
		{Name: "hidden", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ok", nil }},
		{Name: "wait", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(context.Context, json.RawMessage) (string, error) { approved.Add(1); return "ok", nil }},
		{Name: "spare", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) {
			t.Error("unselected spare executed")
			return "", nil
		}},
	}
	for _, d := range tools.NewBuiltinDefinitions(tools.BuiltinOptions{}) {
		if d.Name == "search_tools" {
			defs = append(defs, d)
		}
	}
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: m, Tools: defs, GenerationFingerprint: "search-reopen-v1", Principal: "operator", ResourceScheduler: tools.NewResourceScheduler()}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	receipt := submitOutput(t, s)
	hiddenSelectionNextRequest(t, m.hiddenSelectionModel)
	_, err = s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: receipt.TraceID, ToolNames: []string{"search_tools", "wait"}, ExpectedRevision: s.rt.manager.View().LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	close(first)
	hiddenSelectionNextRequest(t, m.hiddenSelectionModel)
	waitResumeCondition(t, func() bool {
		v := s.rt.manager.View()
		return v.Traces[receipt.TraceID].State == "paused" || terminal(v.Traces[receipt.TraceID].State)
	})
	before := s.rt.manager.View()
	trace := before.Traces[receipt.TraceID]
	cp := before.Checkpoints[trace.CheckpointID]
	if trace.State != "paused" || runs.Load() != 0 || approved.Load() != 0 || len(cp.CallIDs) == 0 {
		t.Fatalf("not paused: %+v", trace)
	}
	pending := s.rt.findTurnSelection(before, cp.Scope, "tools")
	if pending == nil || pending.State != "pending" || !slices.Equal(pending.ToolNames, []string{"hidden", "search_tools", "wait"}) {
		t.Fatalf("pending %+v", pending)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	rebuilt := &searchReopenModel{&hiddenSelectionModel{FakeModel: testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "actual", Name: "hidden", Arguments: `{}`}}}, testkit.Step{Text: "done"}), requests: make(chan hiddenSelectionRequest, 8)}}
	opts.Model = rebuilt
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	after := opened.rt.manager.View()
	if rebuilt.Calls() != 0 || runs.Load() != 0 || approved.Load() != 0 || !reflect.DeepEqual(before.Selections, after.Selections) || !reflect.DeepEqual(before.Turns, after.Turns) {
		t.Fatal("reopen changed selection or executed work")
	}
	fresh := resumeForFreshApproval(t, opened, receipt.TraceID)
	for id := range fresh.Interactions {
		if _, err := opened.RespondInteraction(t.Context(), InteractionResponse{InteractionID: id, Decision: "allowed-once", ExpectedRevision: opened.rt.manager.View().LastSeq}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := opened.Resume(t.Context(), ResumeCommand{TraceID: receipt.TraceID, ExpectedRevision: opened.rt.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
	req := hiddenSelectionNextRequest(t, rebuilt.hiddenSelectionModel)
	slices.Sort(req.tools)
	if !slices.Equal(req.tools, []string{"hidden", "search_tools", "wait"}) {
		t.Fatalf("restored inventory %v", req.tools)
	}
	waitResumeCondition(t, func() bool { return terminal(opened.rt.manager.View().Traces[receipt.TraceID].State) })
	final := opened.rt.manager.View()
	if runs.Load() != 1 || approved.Load() != 1 || rebuilt.Calls() != 2 || final.Traces[receipt.TraceID].Usage.ToolExecutions != 3 {
		t.Fatalf("runs=%d approved=%d model=%d usage=%+v", runs.Load(), approved.Load(), rebuilt.Calls(), final.Traces[receipt.TraceID].Usage)
	}
	searches := 0
	for _, call := range final.Calls {
		if call.Call.Name == "search_tools" {
			searches++
			if call.Observation == nil || call.Observation.Status != "succeeded" {
				t.Fatal("search observation missing")
			}
		}
	}
	if searches != 1 {
		t.Fatalf("search executed again: %d", searches)
	}
}
