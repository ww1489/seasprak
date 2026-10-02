package workflowagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
)

const workflowTodoBackend = "workflow-journal-v1"

// workflowTodos owns only this run's journal. A host TODO backend replaces it
// completely; its effects are never mirrored into private workflow TODO state.
type workflowTodos struct{ owner *WorkflowAgent }

type todoUpdate struct {
	CallID       string          `json:"callId"`
	InvocationID string          `json:"invocationId"`
	FrozenHash   string          `json:"frozenHash"`
	Version      uint64          `json:"version"`
	Content      json.RawMessage `json:"content"`
}

func todoBackendIdentity(backend agent.TodoOperations) string {
	if backend == nil {
		return workflowTodoBackend
	}
	if _, ok := backend.(*workflowTodos); ok {
		return workflowTodoBackend
	}
	return "host-injected"
}

func (b *workflowTodos) Update(ctx context.Context, request agent.AuthorizedTodo) (agent.TodoEffect, error) {
	w := b.owner
	f := request.Authorization.Frozen.Clone()
	if !scopeRoot(f.Scope, w.opts.RunID) || request.InvocationID == "" || request.InvocationID != f.Scope.InvocationID || !bytes.Equal(request.Content, f.FinalArguments) || f.Tool != "write_todos" || f.BackendID != "todo-operations" {
		return agent.TodoEffect{}, product.NewError(product.CodePermissionDenied, "TODO request differs from its workflow authorization")
	}
	// Validate calls this run's live validator. Consume once outside the owner
	// mutex, then recheck the committed call and current policy under that mutex.
	if err := request.Authorization.Validate(ctx); err != nil {
		return agent.TodoEffect{}, todoPrecommitError(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return agent.TodoEffect{}, todoPrecommitError(err)
	}
	if err := w.writableLocked(); err != nil {
		return agent.TodoEffect{}, err
	}
	if w.active == nil || w.state.Run.State != "running" || w.state.Run.ExecutionID != w.active.scope.ExecutionID || !w.state.validNodeScope(f.Scope) || f.Generation != w.binding {
		return agent.TodoEffect{}, product.NewError(product.CodeStateConflict, "TODO workflow scope is no longer active")
	}
	if err := w.active.ctx.Err(); err != nil {
		return agent.TodoEffect{}, todoPrecommitError(err)
	}
	decision, err := w.policyLocked(ctx, f, true)
	if err != nil {
		return agent.TodoEffect{}, todoPrecommitError(err)
	}
	if decision != agent.DecisionAllow {
		return agent.TodoEffect{}, product.NewError(product.CodePermissionDenied, "TODO workflow execution is no longer allowed")
	}
	u := todoUpdate{CallID: f.CallID, InvocationID: request.InvocationID, FrozenHash: f.Hash, Version: w.state.Todos[request.InvocationID].Version + 1, Content: append(json.RawMessage(nil), request.Content...)}
	if err := w.state.validateTodo(u); err != nil {
		return agent.TodoEffect{}, product.NewError(product.CodeStateConflict, "TODO update does not match a unique claimed workflow call")
	}
	// Await Append's definitive result even if the caller cancels while it is
	// in flight. An unacknowledged Append is storage_unavailable, never no-effect.
	if err := w.commitLocked(ctx, []storage.Record{record("workflow_todo", u.CallID, u)}, nil); err != nil {
		return agent.TodoEffect{}, err
	}
	return agent.TodoEffect{Version: strconv.FormatUint(u.Version, 10), Content: string(u.Content), Confirmed: true}, nil
}

func todoPrecommitError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(product.NewError(product.CodeStateConflict, "TODO cancelled before commit"), err)
	}
	return err
}

func (s runState) validateTodo(u todoUpdate) error {
	call, ok := s.Calls[u.CallID]
	f, found := s.Frozen["execution:"+u.CallID]
	hash, err := f.Digest()
	n := s.Nodes[f.NodeExecutionID]
	_, duplicate := s.TodoUpdates[u.CallID]
	if s.Initial.Manifest.TodoBackend != workflowTodoBackend || duplicate || !ok || !found || !call.Claimed || call.Observation != nil || !s.validNodeScope(f.Scope) || call.Scope != f.Scope || n.State != "accepted" || n.Kind != "tool" || n.ToolCallID != u.CallID || s.Run.State != "running" || s.Run.ExecutionStopped || f.CallID != u.CallID || f.Tool != "write_todos" || call.Call.Name != f.Tool || f.BackendID != "todo-operations" || err != nil || hash != f.Hash || f.Hash != u.FrozenHash || u.InvocationID == "" || call.Scope.InvocationID != u.InvocationID || !bytes.Equal(f.FinalArguments, u.Content) || !json.Valid(u.Content) || u.Version == 0 || u.Version != s.Todos[u.InvocationID].Version+1 {
		return invalidRecord()
	}
	return nil
}
