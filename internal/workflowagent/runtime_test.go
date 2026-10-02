package workflowagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func output(node, field string) WorkflowValue {
	return WorkflowValue{Ref: &WorkflowRef{Node: node, Field: field}}
}
func modelThenTool() WorkflowDefinition {
	return WorkflowDefinition{Name: "model-check", Version: "v1", Source: "test", FormatVersion: WorkflowFormatV1, Resumable: true, InputSchema: json.RawMessage(`{"type":"object"}`), Nodes: []WorkflowNode{{ID: "s", Type: "start"}, {ID: "m", Type: "model", Model: "chosen", Prompt: "process input"}, {ID: "t", Type: "tool", Tool: "echo", Inputs: map[string]WorkflowValue{"q": output("m", "text")}}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"result": output("t", "result")}}}, Edges: []WorkflowEdge{{From: "s", To: "m"}, {From: "m", To: "t"}, {From: "t", To: "e"}}}
}
func toolOnly() WorkflowDefinition {
	d := modelThenTool()
	d.Name = "tool-only"
	d.Nodes = append(d.Nodes[:1], d.Nodes[2:]...)
	d.Nodes[1].Inputs = map[string]WorkflowValue{"q": {Literal: json.RawMessage(`"fixed"`)}}
	d.Edges = []WorkflowEdge{{From: "s", To: "t"}, {From: "t", To: "e"}}
	return d
}
func testOptions(t *testing.T, def WorkflowDefinition, m model.AgenticModel, calls *atomic.Int32) WorkflowOptions {
	t.Helper()
	return WorkflowOptions{Workspace: t.TempDir(), StateRoot: t.TempDir(), RunID: agent.MustID(), Definition: def, Models: map[string]model.AgenticModel{"chosen": m}, Principal: "local", GenerationFingerprint: "test-v1", Tools: []tools.Definition{{Name: "echo", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(_ context.Context, raw json.RawMessage) (string, error) { calls.Add(1); return string(raw), nil }}}}
}
func newWorkflow(t *testing.T, opts WorkflowOptions) *WorkflowAgent {
	t.Helper()
	w, err := CreateWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close(context.Background()) })
	return w
}
func submit(t *testing.T, w *WorkflowAgent) {
	t.Helper()
	if _, err := w.SubmitInput(t.Context(), WorkflowInputCommand{Input: json.RawMessage(`{}`), Principal: "local", IdempotencyKey: "input"}); err != nil {
		t.Fatal(err)
	}
}
func waitStopped(t *testing.T, w *WorkflowAgent) WorkflowSnapshot {
	t.Helper()
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		s, err := w.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if s.ExecutionStopped && s.State != "created" {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("run did not exit")
	return WorkflowSnapshot{}
}
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if pe, ok := product.AsError(err); !ok || pe.Code != code {
		t.Fatalf("error %v, want %s", err, code)
	}
}

// These are the four real legacy RED assertions, relocated to the new owner.
func TestStandaloneWorkflowRejectsIncompleteModelBeforeNextTool(t *testing.T) {
	for _, tc := range []struct {
		name string
		step testkit.Step
	}{{"truncated", testkit.Step{Text: "partial", Truncated: true}}, {"missing_finish", testkit.Step{Text: "partial", NoFinish: true}}, {"unexpected_tool", testkit.Step{Text: "partial", ToolCalls: []schema.FunctionToolCall{{CallID: "unrequested", Name: "echo", Arguments: `{}`}}}}, {"wrong_role", testkit.Step{Text: "partial", Role: schema.AgenticRoleTypeUser}}} {
		t.Run(tc.name, func(t *testing.T) {
			var count atomic.Int32
			m := testkit.NewFake(tc.step)
			w := newWorkflow(t, testOptions(t, modelThenTool(), m, &count))
			submit(t, w)
			s := waitStopped(t, w)
			if s.State != "failed" || s.ErrorCode != product.CodeInvalidArgument {
				t.Fatalf("invalid model accepted: %+v", s)
			}
			var node NodeRun
			for _, n := range s.WorkflowNodes {
				if n.NodeID == "m" {
					node = n
				}
				if n.NodeID == "t" {
					t.Error("downstream admitted")
				}
			}
			if node.State != "failed" || node.ErrorCode != product.CodeInvalidArgument {
				t.Fatalf("node %+v", node)
			}
			if m.Calls() != 1 || count.Load() != 0 {
				t.Fatalf("actual calls model=%d tool=%d, want 1/0", m.Calls(), count.Load())
			}
			if s.Usage.LogicalModelCalls != 1 || s.Usage.TransportRequests != 1 || s.Usage.ToolExecutions != 0 {
				t.Fatalf("occupancy %+v", s.Usage)
			}
		})
	}
}

func TestWorkflowUsesControlledToolWithoutCodeModel(t *testing.T) {
	var calls atomic.Int32
	w := newWorkflow(t, testOptions(t, toolOnly(), nil, &calls))
	submit(t, w)
	s := waitStopped(t, w)
	if s.State != "completed" || calls.Load() != 1 || s.Usage.ToolExecutions != 1 || s.Usage.LogicalModelCalls != 0 {
		t.Fatalf("snapshot %+v calls %d", s, calls.Load())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, call := range w.state.Calls {
		if !call.Claimed || call.Observation == nil || call.Scope.SessionID != "" || call.Scope.TraceID != "" || call.Scope.TurnID != "" || call.Scope.WorkflowRunID != s.RunID {
			t.Fatalf("call %+v", call)
		}
	}
	for _, f := range w.state.Frozen {
		if f.Origin != "workflow_node" || f.NodeExecutionID == "" || f.PolicyRef == "" || f.DefinitionRef == "" || f.BindingRef != w.binding {
			t.Fatalf("frozen %+v", f)
		}
	}
}
