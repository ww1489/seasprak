package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

func independentWorkflowProjectionFixture() workflowagent.WorkflowSnapshot {
	return workflowagent.WorkflowSnapshot{
		RunID: "workflow-one", DefinitionName: "trusted-flow", DefinitionVersion: "v1", State: "paused",
		Revision: 7, DurableSeq: 5, Cursor: "WzIsIndvcmtmbG93Iiwid29ya2Zsb3ctb25lIiwiNSJd", InstanceID: "current-instance",
		ExecutionStopped: true, CanResume: true, BindingVersion: "PRIVATE_BINDING", Workspace: "PRIVATE_WORKSPACE",
		WorkflowNodes: map[string]workflowagent.NodeRun{
			"b": {ID: "node-b", NodeID: "effect", Kind: "tool", State: "waiting", ToolCallID: "PRIVATE_CALL", Result: "PRIVATE_NODE_RESULT", ErrorCode: "PRIVATE_NODE_ERROR"},
			"a": {ID: "node-a", NodeID: "generation", Kind: "model", State: "completed", Result: "PRIVATE_MODEL_TEXT"},
		},
		Interactions: map[string]workflowagent.WorkflowInteraction{
			"ready":    {ID: "ready", NodeExecutionID: "node-b", ToolCallID: "PRIVATE_APPROVAL_CALL", Question: "Approve once?", Options: []string{"allowed-once", "rejected", "cancelled", "PRIVATE_OPTION"}, InstanceID: "current-instance", State: "ready"},
			"old":      {ID: "old", NodeExecutionID: "node-b", Question: "PRIVATE_OLD_INSTANCE", Options: []string{"allowed-once"}, InstanceID: "old-instance", State: "ready"},
			"answered": {ID: "answered", NodeExecutionID: "node-b", Question: "PRIVATE_ANSWERED", Options: []string{"allowed-once"}, InstanceID: "current-instance", State: "decided"},
		},
		Result: json.RawMessage(`{"candidate":"PRIVATE_UNCOMPLETED_RESULT"}`), ErrorCode: "PRIVATE_ERROR_DETAIL",
	}
}

func TestIndependentWorkflowSnapshotUsesOnlyPublicRunFields(t *testing.T) {
	source := independentWorkflowProjectionFixture()
	projected, err := projectWorkflowSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE_") || strings.Contains(string(raw), "sessionId") || strings.Contains(string(raw), "traceId") {
		t.Fatalf("private or Code fields entered independent snapshot: %s", raw)
	}
	var decoded struct {
		RunID         string `json:"runId"`
		DurableSeq    string `json:"durableSeq"`
		ErrorCode     string `json:"errorCode"`
		WorkflowNodes []struct {
			NodeExecutionID string `json:"nodeExecutionId"`
		} `json:"workflowNodes"`
		Interactions []struct {
			InteractionID string `json:"interactionId"`
		} `json:"interactions"`
	}
	if json.Unmarshal(raw, &decoded) != nil || decoded.RunID != source.RunID || decoded.DurableSeq != "5" || decoded.ErrorCode != product.CodeInternal || len(decoded.WorkflowNodes) != 2 || decoded.WorkflowNodes[0].NodeExecutionID != "node-a" || len(decoded.Interactions) != 1 || decoded.Interactions[0].InteractionID != "ready" {
		t.Fatalf("wrong public shape: %s", raw)
	}
}

func TestIndependentWorkflowProjectionOwnsOnlyCompletedEndResult(t *testing.T) {
	source := independentWorkflowProjectionFixture()
	source.State = "completed"
	source.Result = json.RawMessage(`{"answer":"selected-end-output"}`)
	projected, err := projectWorkflowSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	source.Result[0] = 'x'
	raw, _ := json.Marshal(projected)
	if !strings.Contains(string(raw), `"result":{"answer":"selected-end-output"}`) || strings.Contains(string(raw), "PRIVATE_") {
		t.Fatalf("completed result aliased or leaked fields: %s", raw)
	}
}

func TestIndependentWorkflowFactorySnapshotPassesPublicProjection(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	run, err := workflowagent.CreateWorkflowAgent(ctx, workflowagent.WorkflowOptions{
		Workspace: t.TempDir(), StateRoot: t.TempDir(), RunID: "public-run",
		Definition: startupEchoWorkflow("public-flow"), Principal: localPrincipal, GenerationFingerprint: "projection-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close(context.Background())
	initial, err := run.Snapshot(ctx)
	if err != nil || initial.State != "created" || initial.Usage.LogicalModelCalls != 0 {
		t.Fatalf("factory executed or failed: state=%s err=%v", initial.State, err)
	}
	if _, err = projectWorkflowSnapshot(initial); err != nil {
		t.Fatal(err)
	}
	sub, err := run.SubscribeFrom(ctx, workflowagent.WorkflowSubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	_, err = run.SubmitInput(ctx, workflowagent.WorkflowInputCommand{Input: json.RawMessage(`{"topic":"chosen"}`), IdempotencyKey: "once", Principal: localPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case _, open := <-sub.Events:
			if !open {
				t.Fatal("observation ended before completion")
			}
			current, err := run.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != "completed" {
				continue
			}
			projected, err := projectWorkflowSnapshot(current)
			if err != nil || !current.ExecutionStopped || current.Usage.LogicalModelCalls != 0 || current.Usage.ToolExecutions != 0 || string(projected.Result) != `{"answer":"chosen"}` {
				t.Fatalf("real independent projection failed: state=%s model=%d tool=%d err=%v", current.State, current.Usage.LogicalModelCalls, current.Usage.ToolExecutions, err)
			}
			return
		case <-ctx.Done():
			t.Fatal("independent run did not complete")
		}
	}
}

func TestIndependentWorkflowProjectionRejectsUnsafePosition(t *testing.T) {
	for _, change := range []func(*workflowagent.WorkflowSnapshot){
		func(s *workflowagent.WorkflowSnapshot) { s.Revision = 1 << 53 },
		func(s *workflowagent.WorkflowSnapshot) { s.Cursor = "WzIsIndvcmtmbG93Iiwib3RoZXIiLCI1Il0" },
		func(s *workflowagent.WorkflowSnapshot) { s.DurableSeq++ },
		func(s *workflowagent.WorkflowSnapshot) { s.RunID = "../foreign" },
	} {
		source := independentWorkflowProjectionFixture()
		change(&source)
		_, err := projectWorkflowSnapshot(source)
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInternal {
			t.Errorf("unsafe projection error=%v, want internal_error", err)
		}
	}
}
