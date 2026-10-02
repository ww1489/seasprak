package consumer_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ww1489/seasprak/sdk"
)

func constantWorkflow() sdk.WorkflowDefinition {
	return sdk.WorkflowDefinition{Name: "constant", Version: "v1", Source: "consumer", FormatVersion: sdk.WorkflowFormatV1,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`),
		Nodes:       []sdk.WorkflowNode{{ID: "s", Type: "start"}, {ID: "e", Type: "end", Inputs: map[string]sdk.WorkflowValue{"name": {Ref: &sdk.WorkflowRef{Node: "s", Field: "name"}}}}},
		Edges:       []sdk.WorkflowEdge{{From: "s", To: "e"}}, Resumable: true}
}

func workflowErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	if pe, ok := sdk.AsError(err); !ok || pe.Code != code {
		t.Fatalf("error %v, want %s", err, code)
	}
}

func waitConsumerWorkflow(t *testing.T, w *sdk.WorkflowAgent) sdk.WorkflowSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		v, err := w.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if v.ExecutionStopped && (v.State == "completed" || v.State == "failed" || v.State == "paused" || v.State == "cancelled") {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("workflow did not stop")
	return sdk.WorkflowSnapshot{}
}

func TestWorkflowConsumerFactoryAndSingleStructuredInput(t *testing.T) {
	opts := sdk.WorkflowOptions{Workspace: t.TempDir(), StateRoot: t.TempDir(), RunID: "consumer-workflow", Definition: constantWorkflow(), Principal: "local", GenerationFingerprint: "consumer-v1"}
	w, err := sdk.CreateWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	initial, err := w.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if initial.State != "created" || len(initial.WorkflowNodes) != 0 || initial.Usage.LogicalModelCalls != 0 {
		t.Fatalf("create executed: %+v", initial)
	}
	for _, raw := range []string{`{"name":3}`, `{}`, `"text"`, `[]`, `{"name":"a"} {}`} {
		_, err := w.SubmitInput(t.Context(), sdk.WorkflowInputCommand{Input: json.RawMessage(raw), IdempotencyKey: "bad", Principal: "local"})
		workflowErrorCode(t, err, sdk.CodeInvalidArgument)
	}
	v, _ := w.Snapshot(t.Context())
	if v.Revision != initial.Revision {
		t.Fatal("invalid input wrote state")
	}
	cmd := sdk.WorkflowInputCommand{Input: json.RawMessage(`{"name":"a"}`), IdempotencyKey: "input-1", Principal: "local"}
	r, err := w.SubmitInput(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	stale := uint64(0)
	cmd.ExpectedRevision = &stale
	again, err := w.SubmitInput(t.Context(), cmd)
	if err != nil || again != r {
		t.Fatalf("replay %+v %+v %v", r, again, err)
	}
	cmd.Input = json.RawMessage(`{"name":"b"}`)
	_, err = w.SubmitInput(t.Context(), cmd)
	workflowErrorCode(t, err, sdk.CodeIdempotencyConflict)
	final := waitConsumerWorkflow(t, w)
	if final.State != "completed" || final.RunID != opts.RunID || string(final.Result) != `{"name":"a"}` || final.Usage.LogicalModelCalls != 0 || final.Usage.ToolExecutions != 0 {
		t.Fatalf("result %+v", final)
	}
	cmd.IdempotencyKey = "another"
	cmd.ExpectedRevision = nil
	_, err = w.SubmitInput(t.Context(), cmd)
	workflowErrorCode(t, err, sdk.CodeStateConflict)
	sub, err := w.SubscribeFrom(t.Context(), sdk.WorkflowSubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for i := uint64(1); i <= final.DurableSeq; i++ {
		select {
		case ev := <-sub.Events:
			if ev.Scope.SessionID != "" || ev.Scope.TraceID != "" || ev.Scope.TurnID != "" || ev.Scope.WorkflowRunID != opts.RunID || ev.DurableSeq == nil || *ev.DurableSeq != i {
				t.Fatalf("wrong workflow event %+v", ev)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("history delivery stalled")
		}
	}
}

func TestWorkflowConsumerReadOnlyAndDiskReopen(t *testing.T) {
	opts := sdk.WorkflowOptions{Workspace: t.TempDir(), StateRoot: t.TempDir(), RunID: "consumer-reopen", Definition: constantWorkflow(), Principal: "local", GenerationFingerprint: "consumer-v1"}
	w, err := sdk.CreateWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.SubmitInput(t.Context(), sdk.WorkflowInputCommand{Input: json.RawMessage(`{"name":"a"}`), IdempotencyKey: "accepted", Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	before := waitConsumerWorkflow(t, w)
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.StateRoot, "workflow-runs", opts.RunID, "journal.jsonl")
	bytesBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ro := opts
	ro.ReadOnly = true
	ro.Definition = sdk.WorkflowDefinition{}
	ro.GenerationFingerprint = ""
	w, err = sdk.OpenWorkflowAgent(t.Context(), ro)
	if err != nil {
		t.Fatal(err)
	}
	v, err := w.Snapshot(t.Context())
	if err != nil || v.State != before.State || v.Revision != before.Revision {
		t.Fatalf("readonly %+v %v", v, err)
	}
	_, err = w.Cancel(t.Context(), sdk.WorkflowControlCommand{Principal: "local"})
	workflowErrorCode(t, err, sdk.CodePermissionDenied)
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	bytesAfter, _ := os.ReadFile(path)
	if string(bytesBefore) != string(bytesAfter) {
		t.Fatal("read-only changed journal")
	}
	wrong := opts
	wrong.Workspace = t.TempDir()
	_, err = sdk.OpenWorkflowAgent(t.Context(), wrong)
	workflowErrorCode(t, err, sdk.CodeStateConflict)
	changed := opts
	changed.GenerationFingerprint = "other"
	_, err = sdk.OpenWorkflowAgent(t.Context(), changed)
	workflowErrorCode(t, err, sdk.CodeIncompatibleVersion)
	w, err = sdk.OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	v, err = w.Snapshot(t.Context())
	if err != nil || v.Revision != before.Revision || v.State != "completed" {
		t.Fatalf("open executed %+v %v", v, err)
	}
	_, err = sdk.CreateWorkflowAgent(t.Context(), opts)
	workflowErrorCode(t, err, sdk.CodeStateConflict)
	bytesAfter, _ = os.ReadFile(path)
	if string(bytesBefore) != string(bytesAfter) {
		t.Fatal("rejected operations changed journal")
	}
}
