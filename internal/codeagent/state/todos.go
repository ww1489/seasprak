package state

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// TodoUpdate is an immutable full replacement for one invocation. CallID is
// also its receipt identity; retrying an older call never rolls back a later list.
type TodoUpdate struct {
	CallID       string          `json:"callId"`
	InvocationID string          `json:"invocationId"`
	FrozenHash   string          `json:"frozenHash"`
	Version      uint64          `json:"version"`
	Content      json.RawMessage `json:"content"`
}

func (u TodoUpdate) Effect() agent.TodoEffect {
	return agent.TodoEffect{Version: strconv.FormatUint(u.Version, 10), Content: string(u.Content), Confirmed: true}
}
func (u TodoUpdate) observation() agent.ToolObservation {
	return agent.ToolObservation{Status: "succeeded", SideEffect: "confirmed", Executed: true, Content: string(u.Content)}
}

// UpdateTodos is called only by the session mailbox after ticket consumption
// and live policy checks. The durable receipt and its original observation are
// one fact, eliminating the gap between an internal effect and result saving.
func (m *Manager) UpdateTodos(ctx context.Context, frozen agent.FrozenExecution) (agent.TodoEffect, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return agent.TodoEffect{}, err
	}
	if frozen.Scope.SessionID != m.sessionID {
		return agent.TodoEffect{}, product.NewError(product.CodePermissionDenied, "TODO session binding mismatch")
	}
	stored, ok := m.view.FrozenExecutions[frozen.ID]
	digest, err := frozen.Digest()
	if !ok || err != nil || digest != frozen.Hash || stored.Hash != frozen.Hash {
		return agent.TodoEffect{}, product.NewError(product.CodePermissionDenied, "TODO execution is not committed")
	}
	if old, ok := m.view.TodoUpdates[frozen.CallID]; ok {
		if old.FrozenHash != frozen.Hash {
			return agent.TodoEffect{}, product.NewError(product.CodeStateConflict, "TODO receipt binding changed")
		}
		return old.Effect(), nil
	}
	u := TodoUpdate{CallID: frozen.CallID, InvocationID: frozen.Scope.InvocationID, FrozenHash: frozen.Hash, Version: m.view.Todos[frozen.Scope.InvocationID].Version + 1, Content: append(json.RawMessage(nil), frozen.FinalArguments...)}
	if err := validateTodoUpdate(m.view, u); err != nil {
		return agent.TodoEffect{}, err
	}
	call := m.view.Calls[u.CallID]
	observation := u.observation()
	call.Observation = &observation
	if _, err := m.commit(ctx, []store.Record{record("todo_update", u.CallID, u)}, nil, []agent.Event{m.event("tool.finished", call.Scope.TraceID, call.Scope.TurnID, call)}); err != nil {
		if m.fault == nil {
			// Manager.commit rejected the candidate before attempting Append.
			return agent.TodoEffect{}, err
		}
		// Append may have reached durable storage even when its reply is lost. Do
		// not expose cancellation or another pre-start code as proof of no effect.
		return agent.TodoEffect{}, product.NewError(product.CodeStorageUnavailable, "TODO commit was not acknowledged; reopen required")
	}
	return u.Effect(), nil
}

func validateTodoUpdate(v *View, u TodoUpdate) error {
	call, ok := v.Calls[u.CallID]
	f, found := v.FrozenExecutions["execution:"+u.CallID]
	hash, err := f.Digest()
	if !ok || !found || !call.Claimed || call.Observation != nil || u.CallID == "" || u.InvocationID == "" || call.Scope != f.Scope || call.Scope.InvocationID != u.InvocationID || f.CallID != u.CallID || f.Tool != "write_todos" || call.Call.Name != f.Tool || f.BackendID != "todo-operations" || err != nil || hash != f.Hash || f.Hash != u.FrozenHash || !bytes.Equal(f.FinalArguments, u.Content) || !json.Valid(u.Content) {
		return product.NewError(product.CodeStateConflict, "TODO update does not match a claimed execution")
	}
	if tr := v.Traces[call.Scope.TraceID]; tr == nil || tr.State != "running" {
		return product.NewError(product.CodeStateConflict, "TODO trace is not running")
	}
	if u.Version != v.Todos[u.InvocationID].Version+1 {
		return product.NewError(product.CodeStateConflict, "TODO version is not consecutive")
	}
	return nil
}

func applyTodoUpdate(v *View, r store.Record) error {
	var u TodoUpdate
	if err := json.Unmarshal(r.Payload, &u); err != nil {
		return err
	}
	if r.ID != u.CallID {
		return product.NewError(product.CodeStateConflict, "TODO receipt identity mismatch")
	}
	if _, exists := v.TodoUpdates[u.CallID]; exists {
		return product.NewError(product.CodeStateConflict, "duplicate TODO receipt")
	}
	if err := validateTodoUpdate(v, u); err != nil {
		return err
	}
	if v.Todos == nil {
		v.Todos = map[string]TodoUpdate{}
	}
	if v.TodoUpdates == nil {
		v.TodoUpdates = map[string]TodoUpdate{}
	}
	v.Todos[u.InvocationID] = u
	v.TodoUpdates[u.CallID] = u
	// Reconstruct the original result from the committed update, including on
	// read-only reopen before the executor could publish its observation. Existing
	// LookupTool and recovery checks then reuse the result without execution.
	call := v.Calls[u.CallID]
	observation := u.observation()
	call.Observation = &observation
	if err := applyLegacyObservation(v, call); err != nil {
		return err
	}
	v.Calls[u.CallID] = call
	return nil
}
