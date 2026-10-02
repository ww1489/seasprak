package workflowagent

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestWorkflowMigrationCompletedToolInterruptedModelDiskResume(t *testing.T) {
	var beforeTool, afterTool, physical atomic.Int32
	gate := make(chan struct{})
	defer close(gate)
	blocked := testkit.NewFake(testkit.Step{Text: "interrupted", Gate: gate}, testkit.Step{Text: "recovered"})
	m := &migrationBlockedObservedModel{observedModel: migrationObservedModel(func(r *http.Request) (*http.Response, error) {
		physical.Add(1)
		return migrationResponse(r), nil
	}), blocked: blocked}
	d := modelThenTool()
	d.Nodes = append(d.Nodes[:1], append([]WorkflowNode{{ID: "before", Type: "tool", Tool: "before", Inputs: map[string]WorkflowValue{"q": migrationLiteral(`"original"`)}}}, d.Nodes[1:]...)...)
	d.Edges = []WorkflowEdge{{From: "s", To: "before"}, {From: "before", To: "m"}, {From: "m", To: "t"}, {From: "t", To: "e"}}
	opts := testOptions(t, d, m, &afterTool)
	opts.Limits = config.Limits{LogicalModelRequests: 4}
	opts.Tools = append(opts.Tools, migrationReadTool("before", func(context.Context, json.RawMessage) (string, error) {
		beforeTool.Add(1)
		return "first-result", nil
	}))
	w := newWorkflow(t, opts)
	submit(t, w)
	deadline := time.Now().Add(5 * time.Second)
	for blocked.Calls() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if blocked.Calls() != 1 || beforeTool.Load() != 1 || afterTool.Load() != 0 || physical.Load() != 2 || m.entered.Load() != 1 {
		t.Fatal("completed predecessor did not reach a blocked model with exactly two requests")
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	original := w.state.clone()
	w.mu.Unlock()
	var toolNode, modelNode NodeRun
	for _, n := range original.Nodes {
		switch n.NodeID {
		case "before":
			toolNode = n
		case "m":
			modelNode = n
		default:
			t.Fatal("successor was admitted before interruption")
		}
	}
	if original.Run.State != "paused" || !original.Run.ExecutionStopped || len(original.Nodes) != 2 || toolNode.State != "completed" || modelNode.State != "accepted" || modelNode.Usage.ModelCallID != modelNode.ID || original.Usage.LogicalModelCalls != 1 || original.Usage.TransportRequests != 2 || original.Usage.ToolExecutions != 1 || len(original.Attempts) != 1 {
		t.Fatal("close lost original nodes or refunded occupied model/tool budget")
	}
	for _, a := range original.Attempts {
		if a.Status != "aborted" || a.Identity.ModelCallID != modelNode.ID || len(original.Requests[a.Identity.ID]) != 2 {
			t.Fatal("interruption lost original logical call or physical requests")
		}
	}
	reopened, err := OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	reopened.mu.Lock()
	afterOpen := reopened.state.clone()
	reopened.mu.Unlock()
	if !reflect.DeepEqual(original, afterOpen) || beforeTool.Load() != 1 || afterTool.Load() != 0 || blocked.Calls() != 1 || physical.Load() != 2 {
		t.Fatal("disk reopen wrote state, auto-resumed, or replaced original identities")
	}
	if _, err := reopened.Resume(t.Context(), WorkflowControlCommand{Principal: "local", IdempotencyKey: "explicit-resume"}); err != nil {
		t.Fatal(err)
	}
	final := waitStopped(t, reopened)
	if final.State != "completed" || beforeTool.Load() != 1 || afterTool.Load() != 1 || blocked.Calls() != 2 || m.entered.Load() != 2 || physical.Load() != 4 || final.Usage.LogicalModelCalls != 1 || final.Usage.TransportRequests != 4 || final.Usage.ToolExecutions != 2 || final.WorkflowNodes[toolNode.ID] != toolNode {
		t.Fatalf("resume repeated predecessor or changed budgets: state=%s code=%s tools=%d/%d model=%d wire=%d usage=%+v", final.State, final.ErrorCode, beforeTool.Load(), afterTool.Load(), blocked.Calls(), physical.Load(), final.Usage)
	}
	recovered := final.WorkflowNodes[modelNode.ID]
	if recovered.State != "completed" || recovered.InvocationID != modelNode.InvocationID || recovered.Usage.ModelCallID != modelNode.ID || recovered.Usage.ModelRequests != 4 || recovered.Result != "recovered" {
		t.Fatal("resume replaced original invocation/node/logical call")
	}
	reopened.mu.Lock()
	defer reopened.mu.Unlock()
	if !reflect.DeepEqual(reopened.state.Calls[toolNode.ToolCallID], original.Calls[toolNode.ToolCallID]) || len(reopened.state.Attempts) != 2 {
		t.Fatal("completed original tool call changed or model terminal attempt duplicated")
	}
	var result map[string]string
	if err := json.Unmarshal(final.Result, &result); err != nil || result["result"] != `{"q":"recovered"}` {
		t.Fatal("explicit recovery did not produce the expected end result")
	}
}
