package workflowagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

type workflowTodoOperation func(context.Context, agent.AuthorizedTodo) (agent.TodoEffect, error)

func (f workflowTodoOperation) Update(ctx context.Context, r agent.AuthorizedTodo) (agent.TodoEffect, error) {
	return f(ctx, r)
}

func TestWorkflowTodosRejectAndCancelHaveNoEffects(t *testing.T) {
	for _, decision := range []string{"rejected", "cancelled", "cancel_run", "approve_then_cancel_run"} {
		t.Run(decision, func(t *testing.T) {
			opts := todoOptions(t, "once")
			w := newWorkflow(t, opts)
			submit(t, w)
			paused := waitStopped(t, w)
			if paused.State != "paused" || len(paused.Interactions) != 1 {
				t.Fatalf("write_todos did not ask: state=%s code=%s", paused.State, paused.ErrorCode)
			}
			if decision == "approve_then_cancel_run" {
				answerTodo(t, w, paused, "allowed-once")
			}
			if decision == "cancel_run" || decision == "approve_then_cancel_run" {
				if _, err := w.Cancel(t.Context(), WorkflowControlCommand{Principal: "local"}); err != nil {
					t.Fatal(err)
				}
			} else {
				answerTodo(t, w, paused, decision)
				if len(todoRecords(loadTodoRun(t, opts), "workflow_todo")) != 0 {
					t.Fatal("answer performed an effect")
				}
				if _, err := w.Resume(t.Context(), WorkflowControlCommand{Principal: "local"}); err != nil {
					t.Fatal(err)
				}
			}
			final := waitStopped(t, w)
			wantState, wantCode, wantObservation := "failed", product.CodePermissionDenied, "denied"
			switch decision {
			case "cancelled":
				wantCode, wantObservation = product.CodeResourceUnavailable, "cancelled"
			case "cancel_run", "approve_then_cancel_run":
				wantState, wantCode, wantObservation = "cancelled", "", "skipped"
			}
			if final.State != wantState || final.ErrorCode != wantCode || !final.ExecutionStopped {
				t.Fatalf("unapproved TODO terminal state/code: got %s/%s want %s/%s", final.State, final.ErrorCode, wantState, wantCode)
			}
			loaded := loadTodoRun(t, opts)
			if final.Usage.ToolExecutions != 0 || len(todoRecords(loaded, "workflow_todo")) != 0 || len(todoRecords(loaded, "workflow_tool_intent")) != 0 || len(final.Observations) != 1 {
				t.Fatalf("unapproved TODO executed: state=%s usage=%+v", final.State, final.Usage)
			}
			for _, observation := range final.Observations {
				if observation.Status != wantObservation || observation.Executed || observation.SideEffect != "none" {
					t.Fatalf("unapproved TODO did not retain zero-effect proof: %+v", observation)
				}
			}
		})
	}
}

func TestWorkflowTodosHostInjectionCompletelyReplacesJournal(t *testing.T) {
	opts := todoOptions(t, "once")
	var updates atomic.Int32
	requests := make(chan agent.AuthorizedTodo, 1)
	reused := make(chan error, 1)
	opts.Operations.Todos = workflowTodoOperation(func(ctx context.Context, request agent.AuthorizedTodo) (agent.TodoEffect, error) {
		if err := request.Authorization.Validate(ctx); err != nil {
			return agent.TodoEffect{}, err
		}
		reused <- request.Authorization.Validate(ctx)
		updates.Add(1)
		requests <- request
		return agent.TodoEffect{Version: "host-v1", Content: "host-result", Confirmed: true}, nil
	})
	w := newWorkflow(t, opts)
	submit(t, w)
	paused := waitStopped(t, w)
	if updates.Load() != 0 {
		t.Fatal("host invoked before approval")
	}
	answerTodo(t, w, paused, "allowed-once")
	if updates.Load() != 0 {
		t.Fatal("answer auto-invoked host")
	}
	if _, err := w.Resume(t.Context(), WorkflowControlCommand{Principal: "local"}); err != nil {
		t.Fatal(err)
	}
	final := waitStopped(t, w)
	if final.State != "completed" || updates.Load() != 1 || final.Usage.ToolExecutions != 1 || string(final.Result) != `{"result":"host-result"}` {
		t.Fatalf("host replacement failed: state=%s code=%s updates=%d", final.State, final.ErrorCode, updates.Load())
	}
	request := <-requests
	frozen := request.Authorization.Frozen
	if frozen.Tool != "write_todos" || frozen.BackendID != "todo-operations" || frozen.Scope.WorkflowRunID != opts.RunID || request.InvocationID != frozen.Scope.InvocationID || string(request.Content) != string(frozen.FinalArguments) {
		t.Fatal("host did not receive the original scoped TODO ticket and arguments")
	}
	requireCode(t, <-reused, product.CodePermissionDenied)
	loaded := loadTodoRun(t, opts)
	if len(todoRecords(loaded, "workflow_todo")) != 0 || len(todoRecords(loaded, "workflow_tool_intent")) != 1 {
		t.Fatal("default journal mirrored a host update")
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	if updates.Load() != 1 || len(todoRecords(loadTodoRun(t, opts), "workflow_todo")) != 0 {
		t.Fatal("reopen invoked or mirrored host")
	}
}

func TestWorkflowTodosManifestOwnershipIsFixed(t *testing.T) {
	for _, injected := range []bool{false, true} {
		name := "workflow-journal-v1"
		if injected {
			name = "host-injected"
		}
		t.Run(name, func(t *testing.T) {
			opts := todoOptions(t, "")
			var updates atomic.Int32
			host := workflowTodoOperation(func(ctx context.Context, request agent.AuthorizedTodo) (agent.TodoEffect, error) {
				if err := request.Authorization.Validate(ctx); err != nil {
					return agent.TodoEffect{}, err
				}
				updates.Add(1)
				return agent.TodoEffect{Content: "host", Confirmed: true}, nil
			})
			if injected {
				opts.Operations.Todos = host
			}
			w := newWorkflow(t, opts)
			loaded := loadTodoRun(t, opts)
			var initial struct {
				Manifest struct {
					TodoBackend string `json:"todoBackend"`
				} `json:"manifest"`
			}
			if err := json.Unmarshal(loaded.Commits[0].ControlRecords[0].Payload, &initial); err != nil {
				t.Fatal(err)
			}
			if initial.Manifest.TodoBackend != name {
				t.Fatalf("TODO ownership missing from immutable manifest: got %q want %q", initial.Manifest.TodoBackend, name)
			}
			if err := w.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			changed := opts
			if injected {
				changed.Operations.Todos = nil
			} else {
				changed.Operations.Todos = host
			}
			opened, err := OpenWorkflowAgent(t.Context(), changed)
			if opened != nil {
				opened.Close(context.Background())
			}
			requireCode(t, err, product.CodeIncompatibleVersion)
			after := loadTodoRun(t, opts)
			if len(after.Commits) != len(loaded.Commits) || updates.Load() != 0 {
				t.Fatal("ownership-changing Open wrote or executed")
			}
		})
	}
}
