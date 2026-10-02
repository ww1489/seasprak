package web

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/codeagent"
	"github.com/ww1489/seasprak/internal/testkit"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

func httpLiteralWorkflow() workflowagent.WorkflowDefinition {
	return workflowagent.WorkflowDefinition{Name: "constant", Version: "v1", Source: "http-fixture", FormatVersion: workflowagent.WorkflowFormatV1, Resumable: true, InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`), Nodes: []workflowagent.WorkflowNode{
		{ID: "start", Type: "start"}, {ID: "literal", Type: "literal", Inputs: map[string]workflowagent.WorkflowValue{"result": {Literal: json.RawMessage(`"chosen"`)}}},
		{ID: "end", Type: "end", Inputs: map[string]workflowagent.WorkflowValue{"result": {Ref: &workflowagent.WorkflowRef{Node: "literal", Field: "result"}}}},
	}, Edges: []workflowagent.WorkflowEdge{{From: "start", To: "literal"}, {From: "literal", To: "end"}}}
}
func configureHTTPWorkflow(t *testing.T, conf Config, definitions ...workflowagent.WorkflowDefinition) {
	t.Helper()
	raw, err := os.ReadFile(conf.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var startup startupConfig
	if err := json.Unmarshal(raw, &startup); err != nil {
		t.Fatal(err)
	}
	startup.Profile = codeagent.ProfileMemory
	for _, definition := range definitions {
		startup.Workflows = append(startup.Workflows, startupWorkflow{definition})
	}
	raw, err = json.Marshal(startup)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf.ConfigPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func workflowHTTPServer(t *testing.T, conf Config) (*Server, string, *testkit.FakeModel) {
	t.Helper()
	fake := testkit.NewFake()
	testOptions = func(o *codeagent.Options) { o.Model = fake }
	server, err := Start(t.Context(), conf, nil)
	testOptions = nil
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close(); _ = server.Wait() })
	token, err := os.ReadFile(server.TokenPath())
	if err != nil {
		t.Fatal(err)
	}
	return server, string(token), fake
}
func workflowCreateBody(conf Config) map[string]any {
	return map[string]any{"workspace": conf.Workspace, "workflow": "constant", "version": "v1", "input": map[string]any{"name": "one"}}
}
func pollWorkflowHTTP(t *testing.T, server *Server, token, rid string, done func(map[string]any) bool) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		status, out := call(t, server, token, "GET", "/v1/workflow-runs/"+rid+"/snapshot", "", nil)
		if status != 200 {
			t.Fatalf("snapshot status=%d %v", status, out)
		}
		if done(out) {
			return out
		}
		select {
		case <-ctx.Done():
			t.Fatalf("workflow condition not reached: %v", out)
		case <-time.After(time.Millisecond):
		}
	}
}
func TestWorkflowHTTPIndependentCreateAndOriginalReceipt(t *testing.T) {
	conf := testConfig(t)
	configureHTTPWorkflow(t, conf, httpLiteralWorkflow())
	server, token, fake := workflowHTTPServer(t, conf)
	status, inventory := call(t, server, token, "GET", "/v1/workflows", "", nil)
	workflows, _ := inventory["workflows"].([]any)
	if status != 200 || len(workflows) != 1 {
		t.Fatalf("global inventory status=%d %v", status, inventory)
	}
	status, first := call(t, server, token, "POST", "/v1/workflow-runs", "same", workflowCreateBody(conf))
	rid, _ := first["runId"].(string)
	if status != 202 || rid == "" {
		t.Fatalf("workflow create status=%d %v", status, first)
	}
	completed := pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["state"] == "completed" && v["executionStopped"] == true })
	if !reflect.DeepEqual(completed["result"], map[string]any{"result": "chosen"}) {
		t.Fatalf("selected result=%v", completed["result"])
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			status, again := call(t, server, token, "POST", "/v1/workflow-runs", "same", workflowCreateBody(conf))
			if status != 202 || !reflect.DeepEqual(first, again) {
				t.Error("parallel retry changed original receipt")
			}
		})
	}
	wg.Wait()
	changed := workflowCreateBody(conf)
	changed["input"] = map[string]any{"name": "two"}
	status, out := call(t, server, token, "POST", "/v1/workflow-runs", "same", changed)
	if status != 409 || out["error"].(map[string]any)["code"] != "idempotency_conflict" {
		t.Fatalf("changed request status=%d %v", status, out)
	}
	if fake.Calls() != 0 {
		t.Fatalf("constant workflow invoked Code model %d", fake.Calls())
	}
	if _, err := os.Stat(filepath.Join(conf.StateRoot, "sessions")); !os.IsNotExist(err) {
		t.Fatal("workflow creation created Code directory")
	}
	for _, v := range []map[string]any{first, completed, inventory} {
		raw, _ := json.Marshal(v)
		for _, field := range []string{`"sessionId"`, `"traceId"`, `"toolCallId"`, `"bindingVersion"`, `"principal"`, `"policy"`, `"manifest"`} {
			if strings.Contains(string(raw), field) {
				t.Fatalf("HTTP leaked field %s", field)
			}
		}
	}
	// The same HTTP key belongs to separate Code creation semantics.
	status, code := call(t, server, token, "POST", "/v1/sessions", "same", map[string]any{"workspace": conf.Workspace})
	if status != 201 || code["sessionId"] == "" {
		t.Fatalf("Code key namespace status=%d %v", status, code)
	}
	for _, target := range []string{"constant", "workflow:constant"} {
		status, out := call(t, server, token, "POST", "/v1/sessions/"+code["sessionId"].(string)+"/inputs", "old-target-"+target, map[string]any{"kind": "prompt", "targetAgent": target, "content": []map[string]string{{"type": "text", "text": "must not execute workflow through Code"}}})
		assertWorkflowError(t, status, out, 422, "unsupported_capability")
	}
	if fake.Calls() != 0 {
		t.Fatal("retired Workflow target invoked Code model")
	}
	server.Close()
	if err := server.Wait(); err != nil {
		t.Fatal(err)
	}
	reopened, token, _ := workflowHTTPServer(t, conf)
	status, again := call(t, reopened, token, "POST", "/v1/workflow-runs", "same", workflowCreateBody(conf))
	if status != 202 || !reflect.DeepEqual(first, again) {
		t.Fatal("restart changed original workflow receipt")
	}
}
func TestWorkflowHTTPBrowseHasZeroExecutionOrWriter(t *testing.T) {
	conf := testConfig(t)
	configureHTTPWorkflow(t, conf, httpLiteralWorkflow())
	server, token, _ := workflowHTTPServer(t, conf)
	status, initial := call(t, server, token, "POST", "/v1/workflow-runs", "create", workflowCreateBody(conf))
	if status != 202 {
		t.Fatalf("create status=%d", status)
	}
	rid := initial["runId"].(string)
	pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["executionStopped"] == true })
	server.Close()
	if err := server.Wait(); err != nil {
		t.Fatal(err)
	}
	server, token, fake := workflowHTTPServer(t, conf)
	journal := filepath.Join(conf.StateRoot, "workflow-runs", rid, "journal.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/workflows", "/v1/workflow-runs", "/v1/workflow-runs/" + rid, "/v1/workflow-runs/" + rid + "/snapshot", "/v1/workflow-runs/" + rid + "/operations/missing"} {
		status, _ := call(t, server, token, "GET", path, "", nil)
		want := 200
		if strings.HasSuffix(path, "missing") {
			want = 404
		}
		if status != want {
			t.Fatalf("GET %s status=%d", path, status)
		}
	}
	frames, status := readFrames(t, server, token, "/v1/workflow-runs/"+rid+"/events", "", func(f []sseFrame) bool { return f[len(f)-1].event == "end" })
	if status != 200 || !strings.Contains(frames[0].data, `"live":false`) {
		t.Fatalf("history SSE status=%d", status)
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(before, after) || fake.Calls() != 0 {
		t.Fatal("browse wrote journal or executed model")
	}
	for range 2 {
		status, _ = call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/open", "", map[string]any{})
		if status != 200 {
			t.Fatalf("explicit open status=%d", status)
		}
	}
	frames, status = readFrames(t, server, token, "/v1/workflow-runs/"+rid+"/events", "", func(f []sseFrame) bool { return f[0].event == "ready" })
	if status != 200 || !strings.Contains(frames[0].data, `"live":true`) {
		t.Fatal("explicit open did not own writer")
	}
	after, err = os.ReadFile(journal)
	if err != nil || !bytes.Equal(before, after) || fake.Calls() != 0 {
		t.Fatal("open resumed or changed completed journal")
	}
}
func TestWorkflowHTTPPrevalidationHasZeroSideEffects(t *testing.T) {
	conf := testConfig(t)
	configureHTTPWorkflow(t, conf, httpLiteralWorkflow())
	server, token, fake := workflowHTTPServer(t, conf)
	for _, bad := range []map[string]any{
		{"workspace": t.TempDir(), "workflow": "constant", "version": "v1", "input": map[string]any{"name": "one"}},
		{"workspace": conf.Workspace, "workflow": "missing", "version": "v1", "input": map[string]any{"name": "one"}},
		{"workspace": conf.Workspace, "workflow": "constant", "version": "v2", "input": map[string]any{"name": "one"}},
		{"workspace": conf.Workspace, "workflow": "constant", "version": "v1", "input": map[string]any{}},
		{"workspace": conf.Workspace, "workflow": "constant", "version": "v1", "input": []any{}},
	} {
		status, _ := call(t, server, token, "POST", "/v1/workflow-runs", "bad", bad)
		if status != 400 && status != 403 {
			t.Fatalf("invalid input status=%d", status)
		}
	}
	if status, _ := call(t, server, token, "POST", "/v1/workflow-runs", "", workflowCreateBody(conf)); status != 400 {
		t.Fatalf("missing key status=%d", status)
	}
	for _, namespace := range []string{"workflow-runs", "sessions"} {
		if _, err := os.Lstat(filepath.Join(conf.StateRoot, namespace)); !os.IsNotExist(err) {
			t.Fatal("rejection materialized a resource")
		}
	}
	entries, err := os.ReadDir(filepath.Join(conf.StateRoot, "catalog", "creations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			t.Fatal("rejection reserved an ID")
		}
	}
	if fake.Calls() != 0 {
		t.Fatal("invalid workflow invoked model")
	}
}
