package web

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

func workflowToolDefinition(name string) workflowagent.WorkflowDefinition {
	return workflowagent.WorkflowDefinition{Name: "tool", Version: "v1", Source: "http-tool-fixture", FormatVersion: workflowagent.WorkflowFormatV1, Resumable: true, InputSchema: json.RawMessage(`{"type":"object"}`), Nodes: []workflowagent.WorkflowNode{{ID: "start", Type: "start"}, {ID: "tool", Type: "tool", Tool: name, Inputs: map[string]workflowagent.WorkflowValue{"q": {Literal: json.RawMessage(`"private-argument-marker"`)}}}, {ID: "end", Type: "end", Inputs: map[string]workflowagent.WorkflowValue{"result": {Literal: json.RawMessage(`"selected"`)}}}}, Edges: []workflowagent.WorkflowEdge{{From: "start", To: "tool"}, {From: "tool", To: "end"}}}
}
func workflowRuntimeFixture(t *testing.T, def workflowagent.WorkflowDefinition, tool tools.Definition) (runtimeOptions, Config) {
	t.Helper()
	code, _ := catalogOptions(t)
	opts := workflowagent.WorkflowOptions{Workspace: code.Workspace, StateRoot: code.StateRoot, Principal: localPrincipal, Definition: def, GenerationFingerprint: "http-workflow-fixture-v1", Tools: []tools.Definition{tool}}
	return runtimeOptions{Code: code, Workflows: map[string]workflowagent.WorkflowOptions{def.Name + "@" + def.Version: opts}}, Config{Workspace: code.Workspace, StateRoot: code.StateRoot}
}
func runtimeHTTP(t *testing.T, runtime runtimeOptions) (*resourceCatalogs, *Server, string) {
	t.Helper()
	catalogs, err := newResourceCatalogs(runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalogs.Close(context.Background()) })
	server, token := catalogsHTTP(t, catalogs)
	return catalogs, server, token
}
func catalogsHTTP(t *testing.T, catalogs *resourceCatalogs) (*Server, string) {
	t.Helper()
	token := rand.Text()
	httpServer := httptest.NewUnstartedServer(nil)
	httpServer.Config.Handler = authorize(httpServer.Listener.Addr().String(), token, newRoutes(catalogs.code, localPrincipal, catalogs.workflows))
	httpServer.Start()
	t.Cleanup(httpServer.Close)
	return &Server{url: httpServer.URL, options: catalogs.code.opts}, token
}
func toolCreateBody(conf Config) map[string]any {
	return map[string]any{"workspace": conf.Workspace, "workflow": "tool", "version": "v1", "input": map[string]any{}}
}
func readyQuestion(t *testing.T, server *Server, token, rid string) (map[string]any, map[string]any) {
	t.Helper()
	snap := pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["state"] == "paused" && v["executionStopped"] == true })
	questions := snap["interactions"].([]any)
	if len(questions) != 1 {
		t.Fatalf("ready questions=%v", questions)
	}
	return snap, questions[0].(map[string]any)
}
func assertWorkflowError(t *testing.T, status int, out map[string]any, wantStatus int, code string) {
	t.Helper()
	body, _ := out["error"].(map[string]any)
	if status != wantStatus || body["code"] != code {
		t.Fatalf("status=%d err=%v want %d/%s", status, out, wantStatus, code)
	}
}
func TestWorkflowHTTPInstanceAnswerExplicitResumeAndReject(t *testing.T) {
	for _, decision := range []string{"allowed-once", "rejected", "cancelled"} {
		t.Run(decision, func(t *testing.T) {
			var effects atomic.Int32
			tool := tools.Definition{Name: "probe", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "once"}, Run: func(context.Context, json.RawMessage) (string, error) {
				effects.Add(1)
				return "private-node-result-marker", nil
			}}
			runtime, conf := workflowRuntimeFixture(t, workflowToolDefinition("probe"), tool)
			catalogs, server, token := runtimeHTTP(t, runtime)
			status, created := call(t, server, token, "POST", "/v1/workflow-runs", "create", toolCreateBody(conf))
			if status != 202 {
				t.Fatalf("create status=%d", status)
			}
			rid := created["runId"].(string)
			snap, q := readyQuestion(t, server, token, rid)
			iid := q["interactionId"].(string)
			body := map[string]any{"decision": decision, "expectedRevision": snap["revision"], "instanceId": snap["instanceId"]}
			stale := map[string]any{"decision": decision, "expectedRevision": snap["revision"], "instanceId": "old-instance"}
			status, out := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/interactions/"+iid+"/responses", "answer", stale)
			assertWorkflowError(t, status, out, 409, "state_conflict")
			status, receipt := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/interactions/"+iid+"/responses", "answer", body)
			if status != 202 || receipt["target"] != iid || receipt["acceptedCommit"] != float64(0) || receipt["scope"] != "instance" || receipt["instanceId"] != snap["instanceId"] {
				t.Fatalf("answer receipt status=%d %v", status, receipt)
			}
			status, duplicate := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/interactions/"+iid+"/responses", "answer", body)
			if status != 202 || !reflect.DeepEqual(receipt, duplicate) {
				t.Fatal("duplicate answer lost original instance receipt")
			}
			after, err := catalogs.workflows.Snapshot(t.Context(), rid)
			if err != nil || after.Revision != uint64(snap["revision"].(float64)) || after.State != "paused" || !after.ExecutionStopped || effects.Load() != 0 {
				t.Fatal("answer resumed or executed")
			}
			status, op := call(t, server, token, "GET", "/v1/workflow-runs/"+rid+"/operations/"+receipt["operationId"].(string), "", nil)
			if status != 200 || op["state"] != "completed" {
				t.Fatalf("instance operation status=%d %v", status, op)
			}
			resume := map[string]any{"expectedRevision": snap["revision"]}
			status, resumed := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/resume", "resume", resume)
			if status != 202 || resumed["target"] != rid || resumed["scope"] != "durable" {
				t.Fatalf("resume receipt status=%d %v", status, resumed)
			}
			final := pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["executionStopped"] == true })
			status, dup := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/resume", "resume", resume)
			if status != 202 || !reflect.DeepEqual(resumed, dup) {
				t.Fatal("duplicate resume repeated admission")
			}
			if decision == "allowed-once" {
				if effects.Load() != 1 || final["state"] != "completed" {
					t.Fatalf("approved actual effects=%d state=%v", effects.Load(), final["state"])
				}
			} else if effects.Load() != 0 {
				t.Fatal("rejection executed tool")
			}
			status, answerAgain := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/interactions/"+iid+"/responses", "answer", body)
			if status != 202 || !reflect.DeepEqual(receipt, answerAgain) {
				t.Fatalf("answer retry after execution changed original receipt: %d %v", status, answerAgain)
			}
			raw, _ := json.Marshal(final)
			for _, marker := range []string{"private-argument-marker", "private-node-result-marker", "toolCallId", "bindingVersion", "principal"} {
				if strings.Contains(string(raw), marker) {
					t.Fatalf("snapshot leaked %s", marker)
				}
			}
		})
	}
}
func TestWorkflowHTTPApprovalReopenReasksWithoutOldDecision(t *testing.T) {
	var effects atomic.Int32
	tool := tools.Definition{Name: "probe", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "once"}, Run: func(context.Context, json.RawMessage) (string, error) { effects.Add(1); return "done", nil }}
	runtime, conf := workflowRuntimeFixture(t, workflowToolDefinition("probe"), tool)
	catalogs, server, token := runtimeHTTP(t, runtime)
	_, created := call(t, server, token, "POST", "/v1/workflow-runs", "create", toolCreateBody(conf))
	rid := created["runId"].(string)
	snap, q := readyQuestion(t, server, token, rid)
	iid := q["interactionId"].(string)
	original := map[string]any{"decision": "allowed-once", "expectedRevision": snap["revision"], "instanceId": snap["instanceId"]}
	status, _ := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/interactions/"+iid+"/responses", "answer", original)
	if status != 202 {
		t.Fatalf("answer status=%d", status)
	}
	if err := catalogs.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	catalogs, server, token = runtimeHTTP(t, runtime)
	status, opened := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/open", "", map[string]any{})
	if status != 200 || opened["instanceId"] == snap["instanceId"] || len(opened["interactions"].([]any)) != 0 {
		t.Fatal("reopen restored old approval")
	}
	status, out := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/interactions/"+iid+"/responses", "answer", original)
	assertWorkflowError(t, status, out, 409, "state_conflict")
	status, out = call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/interactions/"+iid+"/responses", "answer", map[string]any{"decision": "allowed-once", "expectedRevision": opened["revision"], "instanceId": opened["instanceId"]})
	assertWorkflowError(t, status, out, 404, "not_found")
	status, _ = call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/resume", "reask", map[string]any{"expectedRevision": opened["revision"]})
	if status != 202 {
		t.Fatalf("reask resume status=%d", status)
	}
	fresh, freshQ := readyQuestion(t, server, token, rid)
	if freshQ["interactionId"] == iid || effects.Load() != 0 {
		t.Fatal("reopen reused old decision or executed")
	}
	status, _ = call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/interactions/"+freshQ["interactionId"].(string)+"/responses", "fresh", map[string]any{"decision": "allowed-once", "expectedRevision": fresh["revision"], "instanceId": fresh["instanceId"]})
	if status != 202 {
		t.Fatalf("fresh answer status=%d", status)
	}
	status, _ = call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/resume", "resume", map[string]any{"expectedRevision": fresh["revision"]})
	if status != 202 {
		t.Fatalf("resume status=%d", status)
	}
	pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["state"] == "completed" && v["executionStopped"] == true })
	if effects.Load() != 1 {
		t.Fatal("reopened tool count mismatch")
	}
}
func TestWorkflowHTTPCancelWaitsForExitAndPreservesUnrelatedRun(t *testing.T) {
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var count atomic.Int32
	tool := tools.Definition{Name: "probe", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", Resources: []agent.ExecutionResource{{Identity: "http-fixture"}}}, Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		count.Add(1)
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return "", ctx.Err()
	}}
	runtime, conf := workflowRuntimeFixture(t, workflowToolDefinition("probe"), tool)
	codeRelease := make(chan struct{})
	defer close(codeRelease)
	codeModel := &catalogUncooperativeModel{FakeModel: testkit.NewFake(testkit.Step{Gate: codeRelease, Text: "unrelated Code completed"}), started: make(chan struct{}), cancelled: make(chan struct{})}
	runtime.Code.Model = codeModel
	catalogs, server, token := runtimeHTTP(t, runtime)
	// Independent root with its own controlled tool and no captured cancellation.
	var unrelatedCount atomic.Int32
	unrelatedRelease := make(chan struct{})
	defer close(unrelatedRelease)
	otherOpts := runtime.Workflows["tool@v1"]
	otherOpts.RunID = "unrelated-workflow"
	otherOpts.Tools = append([]tools.Definition(nil), otherOpts.Tools...)
	otherOpts.Tools[0].Run = func(context.Context, json.RawMessage) (string, error) {
		unrelatedCount.Add(1)
		<-unrelatedRelease
		return "other", nil
	}
	unrelated, err := workflowagent.CreateWorkflowAgent(t.Context(), otherOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unrelated.Close(context.Background()) })
	if _, err := unrelated.SubmitInput(t.Context(), workflowagent.WorkflowInputCommand{Input: json.RawMessage(`{}`), Principal: localPrincipal, IdempotencyKey: "other"}); err != nil {
		t.Fatal(err)
	}
	// An independent Code session also remains untouched by this run's control.
	code, err := catalogs.code.Create(t.Context(), CatalogCreateRequest{Workspace: conf.Workspace, ModelRef: DefaultModelRef, IdempotencyKey: "other-code"})
	if err != nil {
		t.Fatal(err)
	}
	codeSession, err := catalogs.code.Writer(t.Context(), code.Snapshot.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	codeInput, err := codeSession.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"unrelated Code running"}`), IdempotencyKey: "unrelated-code-input"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-codeModel.started:
	case <-time.After(5 * time.Second):
		t.Fatal("unrelated Code model did not enter")
	}
	assertCodeUnaffected := func() {
		t.Helper()
		snapshot, err := codeSession.Snapshot(t.Context())
		trace := snapshot.Traces[codeInput.TraceID]
		if err != nil || trace == nil || trace.State != "running" || trace.ExecutionStopped || len(snapshot.Operations) != 0 || trace.Usage.LogicalModelCalls != 1 || codeModel.Calls() != 1 {
			t.Fatal("workflow cancel changed unrelated active Code execution")
		}
		select {
		case <-codeModel.cancelled:
			t.Fatal("workflow cancel reached unrelated Code model context")
		default:
		}
	}
	_, created := call(t, server, token, "POST", "/v1/workflow-runs", "create", toolCreateBody(conf))
	rid := created["runId"].(string)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		snapshot, err := catalogs.workflows.Snapshot(t.Context(), rid)
		t.Fatalf("tool did not enter: state=%s error=%s nodes=%v snapshotErr=%v counts=%d/%d", snapshot.State, snapshot.ErrorCode, snapshot.WorkflowNodes, err, count.Load(), unrelatedCount.Load())
	}
	before, err := catalogs.workflows.Snapshot(t.Context(), rid)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan map[string]any, 1)
	go func() {
		status, out := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/cancel", "cancel", map[string]any{"expectedRevision": before.Revision, "reason": "test"})
		if status != 202 {
			t.Errorf("cancel status=%d %v", status, out)
		}
		result <- out
	}()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not reach tool")
	}
	pending, err := catalogs.workflows.Snapshot(t.Context(), rid)
	if err != nil || pending.State != "cancelling" || pending.ExecutionStopped {
		t.Fatal("cancel intent counted as exit")
	}
	select {
	case <-result:
		t.Fatal("cancel returned completion before actual exit")
	default:
	}
	other, err := unrelated.Snapshot(t.Context())
	if err != nil || other.State != "running" || other.ExecutionStopped {
		t.Fatal("cancel reached unrelated workflow")
	}
	assertCodeUnaffected()
	close(release)
	var receipt map[string]any
	select {
	case receipt = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP cancel did not finish after exit")
	}
	final := pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["state"] == "cancelled" && v["executionStopped"] == true })
	status, again := call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/cancel", "cancel", map[string]any{"expectedRevision": before.Revision, "reason": "test"})
	if status != 202 || !reflect.DeepEqual(receipt, again) {
		t.Fatal("cancel retry changed original receipt")
	}
	status, op := call(t, server, token, "GET", "/v1/workflow-runs/"+rid+"/operations/"+receipt["operationId"].(string), "", nil)
	if status != 200 || op["state"] != "completed" || op["revision"] != final["revision"] {
		t.Fatalf("cancel operation=%v", op)
	}
	assertCodeUnaffected()
	if count.Load() != 1 || unrelatedCount.Load() != 1 {
		t.Fatal("control repeated execution")
	}
}
func TestWorkflowHTTPUnsafeOrMissingRevisionRejectsBeforeWriter(t *testing.T) {
	conf := testConfig(t)
	configureHTTPWorkflow(t, conf, httpLiteralWorkflow())
	server, token, _ := workflowHTTPServer(t, conf)
	for _, action := range []string{"pause", "cancel", "resume"} {
		for _, body := range []map[string]any{{}, {"expectedRevision": uint64(1 << 53)}, {"expectedRevision": -1}, {"expectedRevision": 1.25}} {
			status, out := call(t, server, token, "POST", "/v1/workflow-runs/missing/"+action, "control", body)
			assertWorkflowError(t, status, out, 400, "invalid_argument")
		}
	}
}
func TestWorkflowHTTPDefaultTodoBackendApproval(t *testing.T) {
	conf := testConfig(t)
	def := workflowToolDefinition("write_todos")
	def.Nodes[1].Inputs = map[string]workflowagent.WorkflowValue{"items": {Literal: json.RawMessage(`[{"id":"one","title":"independent TODO","state":"pending"}]`)}}
	configureHTTPWorkflow(t, conf, def)
	raw, _ := os.ReadFile(conf.ConfigPath)
	var startup startupConfig
	_ = json.Unmarshal(raw, &startup)
	startup.Tools = []string{"write_todos"}
	startup.ApprovalTools = []string{"write_todos"}
	raw, _ = json.Marshal(startup)
	if err := os.WriteFile(conf.ConfigPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	server, token, _ := workflowHTTPServer(t, conf)
	status, created := call(t, server, token, "POST", "/v1/workflow-runs", "todo", toolCreateBody(conf))
	if status != 202 {
		t.Fatalf("create status=%d", status)
	}
	rid := created["runId"].(string)
	snapshot, q := readyQuestion(t, server, token, rid)
	journal := filepath.Join(conf.StateRoot, "workflow-runs", rid, "journal.jsonl")
	assertTodoCount := func(want int) {
		t.Helper()
		raw, err := os.ReadFile(journal)
		if err != nil {
			t.Fatal(err)
		}
		n := strings.Count(string(raw), `"type":"workflow_todo"`)
		if n != want {
			t.Fatalf("actual independent TODO commits=%d want=%d", n, want)
		}
	}
	assertTodoCount(0)
	status, _ = call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/interactions/"+q["interactionId"].(string)+"/responses", "answer", map[string]any{"decision": "allowed-once", "expectedRevision": snapshot["revision"], "instanceId": snapshot["instanceId"]})
	if status != 202 {
		t.Fatalf("answer status=%d", status)
	}
	assertTodoCount(0)
	status, _ = call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/resume", "resume", map[string]any{"expectedRevision": snapshot["revision"]})
	if status != 202 {
		t.Fatalf("resume status=%d", status)
	}
	final := pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["executionStopped"] == true })
	if final["state"] != "completed" {
		t.Fatalf("real default TODO backend state=%v code=%v", final["state"], final["errorCode"])
	}
	assertTodoCount(1)
	if _, err := storage.OpenResourceDir(conf.StateRoot, storage.ResourceCode, rid); err == nil {
		t.Fatal("TODO borrowed Code journal")
	}
}
