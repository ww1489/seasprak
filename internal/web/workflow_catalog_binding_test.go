package web

import (
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

func TestWorkflowCatalogFreezesTrustedDefinition(t *testing.T) {
	runtime, conf, _ := modelRuntime(t)
	// Use a literal so the test mutates a reference-bearing map and raw bytes,
	// not the trusted executable model or a test callback.
	opts := runtime.Workflows["constant@v1"]
	opts.Definition = httpLiteralWorkflow()
	runtime.Workflows["constant@v1"] = opts
	_, server, token := runtimeHTTP(t, runtime)
	opts.Definition.Nodes[1].Inputs["result"] = workflowagent.WorkflowValue{Literal: json.RawMessage(`"changed-private-value"`)}
	opts.Definition.InputSchema[0] = '['
	status, inventory := call(t, server, token, "GET", "/v1/workflows", "", nil)
	if status != 200 {
		t.Fatalf("frozen inventory status=%d", status)
	}
	definitions, ok := inventory["workflows"].([]any)
	if !ok || len(definitions) != 1 {
		t.Fatal("caller mutation corrupted frozen inventory")
	}
	if _, ok := definitions[0].(map[string]any)["inputSchema"].(map[string]any); !ok {
		t.Fatal("caller changed frozen schema")
	}
	status, created := call(t, server, token, "POST", "/v1/workflow-runs", "create", workflowCreateBody(conf))
	if status != 202 {
		t.Fatalf("frozen definition create status=%d %v", status, created)
	}
	final := pollWorkflowHTTP(t, server, token, created["runId"].(string), func(v map[string]any) bool { return v["executionStopped"] == true })
	if final["result"].(map[string]any)["result"] != "chosen" {
		t.Fatal("caller mutated immutable trusted definition")
	}
}
func TestWorkflowCatalogListsCorruptCandidateWithoutWriter(t *testing.T) {
	runtime, conf, _ := modelRuntime(t)
	catalogs, server, token := runtimeHTTP(t, runtime)
	if _, err := storage.PrepareResourceDir(conf.StateRoot, storage.ResourceWorkflow, "corrupt-empty"); err != nil {
		t.Fatal(err)
	}
	status, out := call(t, server, token, "GET", "/v1/workflow-runs", "", nil)
	runs, _ := out["runs"].([]any)
	if status != 200 || len(runs) != 1 || runs[0].(map[string]any)["runId"] != "corrupt-empty" || runs[0].(map[string]any)["available"] != false || len(catalogs.workflows.writers) != 0 {
		t.Fatalf("corrupt candidate disappeared or opened writer: %d %v", status, out)
	}
	if _, err := storage.OpenResourceDir(conf.StateRoot, storage.ResourceCode, "corrupt-empty"); err == nil {
		t.Fatal("list confused typed namespace")
	}
}
