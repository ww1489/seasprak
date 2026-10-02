package agent

import (
	"encoding/json"
	"testing"
)

func TestWorkflowEventAndMessageRootsAreExclusive(t *testing.T) {
	for _, tc := range []struct {
		scope EventScope
		valid bool
	}{{EventScope{SessionID: "code"}, true}, {EventScope{WorkflowRunID: "run", NodeExecutionID: "node"}, true}, {EventScope{}, false}, {EventScope{SessionID: "code", WorkflowRunID: "run"}, false}, {EventScope{WorkflowRunID: "run", TraceID: "fake-trace"}, false}} {
		e := Event{SchemaVersion: 1, Type: "fact", Scope: tc.scope}
		if err := e.Validate(); (err == nil) != tc.valid {
			t.Fatalf("scope %+v: %v", tc.scope, err)
		}
	}
	raw, _ := json.Marshal(ExecutionScope{SessionID: "code"})
	var body map[string]any
	json.Unmarshal(raw, &body)
	if _, ok := body["workflowRunId"]; ok {
		t.Fatal("optional field changes old frozen digest")
	}
}
