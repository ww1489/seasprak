package workflowagent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
)

func TestWorkflowTodosVersionsBelongToEachInvocation(t *testing.T) {
	for _, subflow := range []bool{false, true} {
		name := "one_invocation"
		if subflow {
			name = "root_and_child_invocations"
		}
		t.Run(name, func(t *testing.T) {
			opts := todoOptions(t, "")
			d := opts.Definition
			second := d.Nodes[1]
			second.ID = "second"
			second.Inputs = map[string]WorkflowValue{"items": {Literal: json.RawMessage(`[{"title":"second title","state":"completed"}]`)}}
			if subflow {
				child := d
				child.Name = "child-todos"
				opts.Subflows = map[string]WorkflowDefinition{"child-todos@v1": child}
				second = WorkflowNode{ID: "second", Type: WorkflowNodeSubflow, Subflow: "child-todos@v1"}
			}
			end := d.Nodes[2]
			end.Inputs = map[string]WorkflowValue{"result": output("second", "result")}
			d.Nodes = []WorkflowNode{d.Nodes[0], d.Nodes[1], second, end}
			d.Edges = []WorkflowEdge{{From: "s", To: "t"}, {From: "t", To: "second"}, {From: "second", To: "e"}}
			opts.Definition = d
			w := newWorkflow(t, opts)
			submit(t, w)
			final := waitStopped(t, w)
			loaded := loadTodoRun(t, opts)
			if final.State != "completed" || final.Usage.ToolExecutions != 2 || final.Usage.LogicalModelCalls != 0 || len(todoRecords(loaded, "workflow_todo")) != 2 || len(todoRecords(loaded, "workflow_tool_intent")) != 2 {
				t.Fatalf("invocation TODO graph failed: state=%s code=%s usage=%+v", final.State, final.ErrorCode, final.Usage)
			}
			var first, next todoUpdate
			records := todoRecords(loaded, "workflow_todo")
			if err := json.Unmarshal(records[0].Payload, &first); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(records[1].Payload, &next); err != nil {
				t.Fatal(err)
			}
			if first.CallID == next.CallID || first.Version != 1 {
				t.Fatal("TODO call identity/version was reused")
			}
			if subflow {
				if next.Version != 1 || first.InvocationID == next.InvocationID || len(w.state.Todos) != 2 {
					t.Fatal("child TODO overwrote or borrowed the root version")
				}
			} else if next.Version != 2 || first.InvocationID != next.InvocationID || len(w.state.Todos) != 1 || !reflect.DeepEqual(w.state.Todos[first.InvocationID], next) {
				t.Fatal("sequential calls did not keep consecutive invocation-owned versions")
			}
			if !reflect.DeepEqual(w.state.TodoUpdates[first.CallID], first) || !reflect.DeepEqual(w.state.TodoUpdates[next.CallID], next) {
				t.Fatal("later TODO changed an immutable earlier receipt")
			}
			cloned := w.state.clone()
			u := cloned.Todos[first.InvocationID]
			u.Content[0] = '['
			old := cloned.TodoUpdates[first.CallID]
			old.Content[0] = '['
			if !reflect.DeepEqual(w.state.TodoUpdates[first.CallID], first) || !reflect.DeepEqual(w.state.TodoUpdates[next.CallID], next) {
				t.Fatal("state clone exposed mutable TODO content")
			}
		})
	}
}

func TestWorkflowTodosApprovalDiskReopenRequiresFreshAnswer(t *testing.T) {
	opts := todoOptions(t, "once")
	w := newWorkflow(t, opts)
	submit(t, w)
	paused := waitStopped(t, w)
	var originalQuestion WorkflowInteraction
	for _, originalQuestion = range paused.Interactions {
	}
	answerTodo(t, w, paused, "allowed-once")
	before := loadTodoRun(t, opts)
	originalFrozen := todoRecords(before, "workflow_frozen")[0]
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
	if after.InstanceID == paused.InstanceID || len(after.Interactions) != 0 || after.Revision != paused.Revision || len(todoRecords(loadTodoRun(t, opts), "workflow_todo")) != 0 {
		t.Fatal("Open retained approval or executed a TODO")
	}
	_, err = reopened.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: originalQuestion.ID, Decision: "allowed-once", ExpectedRevision: after.Revision, Principal: "local"})
	requireCode(t, err, product.CodeNotFound)
	if _, err := reopened.Resume(t.Context(), WorkflowControlCommand{Principal: "local"}); err != nil {
		t.Fatal(err)
	}
	askedAgain := waitStopped(t, reopened)
	if len(todoRecords(loadTodoRun(t, opts), "workflow_todo")) != 0 || askedAgain.Usage.ToolExecutions != 0 {
		t.Fatal("new instance inherited approval authority")
	}
	answerTodo(t, reopened, askedAgain, "allowed-once")
	if _, err := reopened.Resume(t.Context(), WorkflowControlCommand{Principal: "local"}); err != nil {
		t.Fatal(err)
	}
	final := waitStopped(t, reopened)
	loaded := loadTodoRun(t, opts)
	if final.State != "completed" || final.Usage.ToolExecutions != 1 || len(todoRecords(loaded, "workflow_todo")) != 1 || len(todoRecords(loaded, "workflow_tool_intent")) != 1 || len(todoRecords(loaded, "workflow_call")) != 1 || !reflect.DeepEqual(todoRecords(loaded, "workflow_frozen"), []storage.Record{originalFrozen}) {
		t.Fatal("fresh approval did not execute the original frozen call exactly once")
	}
}

func TestWorkflowTodosRunsInSameWorkspaceHaveIndependentState(t *testing.T) {
	firstOpts := todoOptions(t, "")
	secondOpts := todoOptions(t, "")
	secondOpts.Workspace, secondOpts.StateRoot = firstOpts.Workspace, firstOpts.StateRoot
	first, second := newWorkflow(t, firstOpts), newWorkflow(t, secondOpts)
	submit(t, first)
	if final := waitStopped(t, first); final.State != "completed" {
		t.Fatal("first TODO did not complete")
	}
	if len(second.state.Todos) != 0 || len(second.state.TodoUpdates) != 0 || len(todoRecords(loadTodoRun(t, secondOpts), "workflow_todo")) != 0 {
		t.Fatal("same-workspace run inherited another run's TODOs")
	}
	submit(t, second)
	if final := waitStopped(t, second); final.State != "completed" {
		t.Fatal("second TODO did not complete")
	}
	for _, w := range []*WorkflowAgent{first, second} {
		if len(w.state.Todos) != 1 || len(w.state.TodoUpdates) != 1 || w.state.Usage.ToolExecutions != 1 {
			t.Fatal("runs shared TODO state or budget")
		}
		for _, update := range w.state.TodoUpdates {
			call := w.state.Calls[update.CallID]
			if update.Version != 1 || call.Scope.WorkflowRunID != w.opts.RunID || call.Scope.SessionID != "" || call.Scope.TraceID != "" {
				t.Fatal("run-local TODO identity/version/root was borrowed")
			}
		}
	}
	if first.instance == second.instance || first.binding != second.binding || first.opts.Operations.Todos == second.opts.Operations.Todos {
		t.Fatal("same definition did not retain separate runtime owners")
	}
}
