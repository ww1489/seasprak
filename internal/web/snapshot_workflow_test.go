package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/sessions/state"
)

// Workflow nodes project identity and state only; results, errors and call
// identities stay server-side.
func TestSnapshotProjectsWorkflowNodesWithoutPrivateFields(t *testing.T) {
	snap := emptySnapshot(3)
	if out := projectSnapshot(snap); out.WorkflowNodes == nil || len(out.WorkflowNodes) != 0 {
		t.Fatalf("empty workflowNodes must encode as [] got %#v", out.WorkflowNodes)
	}
	snap.WorkflowNodes = map[string]state.WorkflowNodeRun{
		"inv:t:1": {ID: "inv:t:1", TraceID: "t1", InvocationID: "inv", NodeID: "t", Kind: "tool", State: "waiting", ToolCallID: "call-private-id", Attempt: 1, Result: "result-private-value", Error: "error-private-value", ModelCalls: 2, ApprovalWait: true},
		"inv:m:1": {ID: "inv:m:1", TraceID: "t1", InvocationID: "inv", NodeID: "m", Kind: "model", State: "completed", Result: "model-private-text"},
	}
	out := projectSnapshot(snap)
	want := []workflowNodeDTO{{NodeExecutionID: "inv:m:1", TraceID: "t1", NodeID: "m", Kind: "model", State: "completed"}, {NodeExecutionID: "inv:t:1", TraceID: "t1", NodeID: "t", Kind: "tool", State: "waiting"}}
	if len(out.WorkflowNodes) != len(want) || out.WorkflowNodes[0] != want[0] || out.WorkflowNodes[1] != want[1] {
		t.Fatalf("workflowNodes %+v", out.WorkflowNodes)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private", "toolCallId", "approvalWait", "modelCalls", "invocationId"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("snapshot leaked %q: %s", private, raw)
		}
	}
}
