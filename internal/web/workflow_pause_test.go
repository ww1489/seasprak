package web

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/testkit"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

func TestWorkflowHTTPPauseWaitsThenResumeReusesCompletedTool(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var toolsCalled atomic.Int32
	tool := tools.Definition{Name: "probe", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "none"}, Run: func(context.Context, json.RawMessage) (string, error) {
		toolsCalled.Add(1)
		close(entered)
		<-release
		return "first", nil
	}}
	def := workflowToolDefinition("probe")
	def.Nodes = append(def.Nodes[:2], workflowagent.WorkflowNode{ID: "model", Type: "model", Model: "default", Prompt: "continue once"}, def.Nodes[2])
	def.Nodes[3].Inputs = map[string]workflowagent.WorkflowValue{"result": {Ref: &workflowagent.WorkflowRef{Node: "model", Field: "text"}}}
	def.Edges = []workflowagent.WorkflowEdge{{From: "start", To: "tool"}, {From: "tool", To: "model"}, {From: "model", To: "end"}}
	runtime, conf := workflowRuntimeFixture(t, def, tool)
	fake := testkit.NewFake(testkit.Step{Text: "resumed"})
	opts := runtime.Workflows["tool@v1"]
	opts.Models = map[string]model.AgenticModel{"default": fake}
	runtime.Workflows["tool@v1"] = opts
	catalogs, server, token := runtimeHTTP(t, runtime)
	status, created := call(t, server, token, "POST", "/v1/workflow-runs", "create", toolCreateBody(conf))
	if status != 202 {
		t.Fatalf("create status=%d", status)
	}
	rid := created["runId"].(string)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("tool not entered")
	}
	before, err := catalogs.workflows.Snapshot(t.Context(), rid)
	if err != nil {
		t.Fatal(err)
	}
	response := make(chan map[string]any, 1)
	body := map[string]any{"expectedRevision": before.Revision}
	go func() {
		status, out := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/pause", "pause", body)
		if status != 202 {
			t.Errorf("pause status=%d %v", status, out)
		}
		response <- out
	}()
	pausing := pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["state"] == "pausing" })
	if pausing["executionStopped"] != false || fake.Calls() != 0 {
		t.Fatal("pause intent treated as exit or next node admitted")
	}
	select {
	case <-response:
		t.Fatal("pause returned before real runner exited")
	default:
	}
	close(release)
	var receipt map[string]any
	select {
	case receipt = <-response:
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not complete after tool exit")
	}
	paused := pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["state"] == "paused" && v["executionStopped"] == true })
	if receipt["target"] != rid || receipt["scope"] != "durable" || paused["canResume"] != true {
		t.Fatalf("pause receipt/state=%v/%v", receipt, paused)
	}
	status, again := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/pause", "pause", body)
	if status != 202 || !reflect.DeepEqual(receipt, again) {
		t.Fatal("pause retry changed receipt")
	}
	resume := map[string]any{"expectedRevision": paused["revision"]}
	status, resumed := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/resume", "resume", resume)
	if status != 202 {
		t.Fatalf("resume status=%d", status)
	}
	final := pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["state"] == "completed" && v["executionStopped"] == true })
	if toolsCalled.Load() != 1 || fake.Calls() != 1 || final["result"].(map[string]any)["result"] != "resumed" {
		t.Fatalf("resumed actual calls tool/model=%d/%d", toolsCalled.Load(), fake.Calls())
	}
	status, again = call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/resume", "resume", resume)
	if status != 202 || !reflect.DeepEqual(resumed, again) || fake.Calls() != 1 {
		t.Fatal("resume retry repeated next node")
	}
}
