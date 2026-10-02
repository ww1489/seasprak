package state

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
)

func todoManagerFixture(t *testing.T) (*Manager, store.Store, agent.FrozenExecution) {
	t.Helper()
	s, err := memory.Open("todo-state", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	m, err := NewManager(s, "todo-state")
	if err != nil {
		t.Fatal(err)
	}
	f := agent.FrozenExecution{ID: "execution:todo", CallID: "todo", Scope: agent.ExecutionScope{SessionID: "todo-state", TraceID: "trace", InvocationID: "invocation"}, Tool: "write_todos", BackendID: "todo-operations", FinalArguments: json.RawMessage(`{"items":[]}`)}
	f.Hash, err = f.Digest()
	if err != nil {
		t.Fatal(err)
	}
	call := agent.ToolRecord{Call: agent.FrozenCall{CallID: "todo", Name: "write_todos"}, Scope: f.Scope, Claimed: true}
	_, err = m.commit(t.Context(), []store.Record{record("trace", "trace", TraceState{ID: "trace", State: "running", InvocationID: "invocation"}), record("tool_call", "todo", call), record("frozen_execution", f.ID, f)}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m, s, f
}

func TestTodosStateReceiptReplaysObservationWithoutSecondCommit(t *testing.T) {
	m, s, f := todoManagerFixture(t)
	effect, err := m.UpdateTodos(t.Context(), f)
	if err != nil || !effect.Confirmed || effect.Version != "1" {
		t.Fatal(effect, err)
	}
	v := m.View()
	if v.LastSeq != 2 || len(v.TodoUpdates) != 1 || len(v.Todos) != 1 || v.Calls[f.CallID].Observation == nil || v.TraceHasUnresolvedEffects("trace") {
		t.Fatal("receipt did not atomically publish original result")
	}
	reopened, err := NewManager(s, "todo-state")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v, reopened.View()) {
		t.Fatal("read-only replay failed to recover receipt and observation")
	}
	again, err := reopened.UpdateTodos(t.Context(), f)
	if err != nil || again != effect || reopened.View().LastSeq != 2 {
		t.Fatal("replayed update appended again", err)
	}
	call := v.Calls[f.CallID]
	if err := reopened.SaveCall(t.Context(), call); err != nil || reopened.View().LastSeq != 2 {
		t.Fatal("executor observation was not idempotent", err)
	}
	v.Todos["invocation"].Content[0] = 'x'
	if !json.Valid(reopened.View().Todos["invocation"].Content) {
		t.Fatal("view aliases persisted content")
	}
}
func TestTodosStateRejectsCancellationAndUnclaimedCalls(t *testing.T) {
	m, _, f := todoManagerFixture(t)
	before := m.View()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.UpdateTodos(ctx, f); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	changed := f.Clone()
	changed.Scope.InvocationID = "other"
	if _, err := m.UpdateTodos(t.Context(), changed); err == nil {
		t.Fatal("changed descriptor accepted")
	}
	if !reflect.DeepEqual(before, m.View()) {
		t.Fatal("rejected update wrote state")
	}
	m.view.Calls[f.CallID] = agent.ToolRecord{Call: before.Calls[f.CallID].Call, Scope: f.Scope}
	_, err := m.UpdateTodos(t.Context(), f)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeStateConflict || len(m.View().TodoUpdates) != 0 {
		t.Fatal("unclaimed update was accepted", err)
	}
}
func TestTodosReplayRejectsInvalidReceipts(t *testing.T) {
	for _, field := range []string{"version", "invocation", "content", "hash", "identity"} {
		t.Run(field, func(t *testing.T) {
			m, _, f := todoManagerFixture(t)
			u := TodoUpdate{CallID: f.CallID, InvocationID: f.Scope.InvocationID, FrozenHash: f.Hash, Version: 1, Content: f.FinalArguments}
			switch field {
			case "version":
				u.Version = 2
			case "invocation":
				u.InvocationID = "other"
			case "content":
				u.Content = json.RawMessage(`{"items":[{}]}`)
			case "hash":
				u.FrozenHash = "other"
			case "identity":
				u.CallID = "other"
			}
			before := m.View()
			_, err := m.commit(t.Context(), []store.Record{record("todo_update", f.CallID, u)}, nil, nil)
			pe, ok := product.AsError(err)
			if !ok || pe.Code != product.CodeStateConflict || !reflect.DeepEqual(before, m.View()) {
				t.Fatal("invalid TODO receipt changed state", err)
			}
		})
	}
}
