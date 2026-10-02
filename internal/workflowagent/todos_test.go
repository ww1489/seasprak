package workflowagent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
)

func requireTodoCode(t *testing.T, err error, code string) {
	t.Helper()
	var pe *product.Error
	if !errors.As(err, &pe) || pe.Code != code {
		t.Fatalf("error %v, want %s", err, code)
	}
}

func todoOptions(t *testing.T, grant string) WorkflowOptions {
	t.Helper()
	d := toolOnly()
	d.Name = "todo-owner"
	d.Nodes[1].Tool = "write_todos"
	d.Nodes[1].Inputs = map[string]WorkflowValue{"items": {Literal: json.RawMessage(`[{"id":"original","title":"original title","state":"pending"}]`)}}
	opts := WorkflowOptions{Workspace: t.TempDir(), StateRoot: t.TempDir(), RunID: agent.MustID(), Definition: d, Principal: "local", GenerationFingerprint: "todo-test-v1"}
	for _, def := range tools.NewBuiltinDefinitions(tools.BuiltinOptions{}) {
		if def.Name == "write_todos" {
			def.Execution.RequestedGrantRef = grant
			opts.Tools = []tools.Definition{def}
		}
	}
	if len(opts.Tools) != 1 {
		t.Fatal("real write_todos builtin is missing")
	}
	return opts
}

func loadTodoRun(t *testing.T, opts WorkflowOptions) storage.StoredSession {
	t.Helper()
	backend := opts.Store
	if backend == nil {
		var err error
		backend, err = jsonl.Open(opts.RunID, opts.StateRoot, storage.Header{}, jsonl.Options{ResourceType: storage.ResourceWorkflow, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Close()
	}
	loaded, err := backend.Load(t.Context(), opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func todoRecords(loaded storage.StoredSession, kind string) []storage.Record {
	var records []storage.Record
	for _, c := range loaded.Commits {
		for _, r := range c.ControlRecords {
			if r.Type == kind {
				records = append(records, r)
			}
		}
	}
	return records
}

func answerTodo(t *testing.T, w *WorkflowAgent, snapshot WorkflowSnapshot, decision string) WorkflowOperationReceipt {
	t.Helper()
	if snapshot.State != "paused" || !snapshot.ExecutionStopped || len(snapshot.Interactions) != 1 {
		t.Fatalf("write_todos did not wait for real approval: state=%s code=%s questions=%d", snapshot.State, snapshot.ErrorCode, len(snapshot.Interactions))
	}
	var question WorkflowInteraction
	for _, question = range snapshot.Interactions {
	}
	if question.State != "ready" || question.ToolCallID == "" {
		t.Fatalf("question is not ready: %+v", question)
	}
	receipt, err := w.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: question.ID, Decision: decision, ExpectedRevision: snapshot.Revision, IdempotencyKey: "answer", Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.AcceptedCommit != 0 || receipt.ReceiptScope != "instance" || receipt.InstanceID != snapshot.InstanceID {
		t.Fatalf("answer was not instance-only: %+v", receipt)
	}
	return receipt
}

func TestWorkflowTodosApprovalExplicitResumeCommitsOnce(t *testing.T) {
	opts := todoOptions(t, "once")
	w := newWorkflow(t, opts)
	submit(t, w)
	paused := waitStopped(t, w)
	before := loadTodoRun(t, opts)
	if len(todoRecords(before, "workflow_todo")) != 0 || len(todoRecords(before, "workflow_tool_intent")) != 0 {
		t.Fatal("approval wait performed a TODO effect or claimed a call")
	}
	answerTodo(t, w, paused, "allowed-once")
	afterAnswer, err := w.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if afterAnswer.Revision != paused.Revision || afterAnswer.State != "paused" || len(todoRecords(loadTodoRun(t, opts), "workflow_todo")) != 0 {
		t.Fatal("Answer wrote a durable effect or automatically resumed")
	}
	if _, err := w.Resume(t.Context(), WorkflowControlCommand{IdempotencyKey: "resume", Principal: "local"}); err != nil {
		t.Fatal(err)
	}
	final := waitStopped(t, w)
	loaded := loadTodoRun(t, opts)
	todos, claims := todoRecords(loaded, "workflow_todo"), todoRecords(loaded, "workflow_tool_intent")
	if final.State != "completed" || len(todos) != 1 || len(claims) != 1 || final.Usage.ToolExecutions != 1 || final.Usage.LogicalModelCalls != 0 {
		t.Fatalf("real write_todos delivery missing: state=%s code=%s TODO=%d claims=%d usage=%+v", final.State, final.ErrorCode, len(todos), len(claims), final.Usage)
	}
	if loaded.Header.ResourceType != storage.ResourceWorkflow || loaded.Header.SessionID != "" || loaded.Header.RunID != opts.RunID {
		t.Fatal("TODO journal is not independently workflow-owned")
	}
	var call agent.ToolRecord
	if err := json.Unmarshal(todoRecords(loaded, "workflow_call")[0].Payload, &call); err != nil {
		t.Fatal(err)
	}
	var update struct {
		CallID       string          `json:"callId"`
		InvocationID string          `json:"invocationId"`
		FrozenHash   string          `json:"frozenHash"`
		Version      uint64          `json:"version"`
		Content      json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(todos[0].Payload, &update); err != nil {
		t.Fatal(err)
	}
	if update.CallID != call.Call.CallID || update.InvocationID != call.Scope.InvocationID || update.Version != 1 || string(update.Content) != call.Call.Arguments || call.Scope.WorkflowRunID != opts.RunID || call.Scope.SessionID != "" || call.Scope.TraceID != "" || call.Scope.TurnID != "" {
		t.Fatalf("TODO did not preserve the original call/arguments/root: %+v scope=%+v", update, call.Scope)
	}
	for _, c := range loaded.Commits {
		for _, event := range c.Events {
			if event.Scope.WorkflowRunID != opts.RunID || event.Scope.SessionID != "" || event.Scope.TraceID != "" || event.Scope.TurnID != "" {
				t.Fatal("TODO execution event has a foreign root")
			}
		}
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	after, err := reopened.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "completed" || after.Revision != final.Revision || after.Usage != final.Usage || len(todoRecords(loadTodoRun(t, opts), "workflow_todo")) != 1 {
		t.Fatal("disk reopen changed the completed effect")
	}
	// The original durable resume receipt is idempotent, even after reopen.
	if _, err := reopened.Resume(t.Context(), WorkflowControlCommand{IdempotencyKey: "resume", Principal: "local"}); err != nil {
		t.Fatal(err)
	}
	if len(todoRecords(loadTodoRun(t, opts), "workflow_todo")) != 1 || len(todoRecords(loadTodoRun(t, opts), "workflow_tool_intent")) != 1 {
		t.Fatal("reopen repeated TODO/claim")
	}
}
