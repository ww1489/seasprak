package state

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

type Manager struct {
	mu        sync.Mutex
	store     store.Store
	sessionID string
	view      *View
	fault     error
}

func NewManager(store store.Store, id string) (*Manager, error) {
	stored, err := store.Load(context.Background(), id)
	if err != nil {
		return nil, err
	}
	v := emptyView()
	v.RepairRequired = stored.RepairRequired
	for _, c := range stored.Commits {
		if err := applyCommit(v, c); err != nil {
			return nil, err
		}
	}
	return &Manager{store: store, sessionID: id, view: v}, nil
}
func emptyView() *View {
	return &View{BranchID: "main", Traces: map[string]*TraceState{}, Inputs: map[string]*InputState{}, Idem: map[string]idemRecord{}, Turns: map[string]agent.TurnRecord{}, Calls: map[string]agent.ToolRecord{}}
}
func clone[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}
func (m *Manager) View() View   { m.mu.Lock(); defer m.mu.Unlock(); return clone(*m.view) }
func (m *Manager) Fault() error { m.mu.Lock(); defer m.mu.Unlock(); return m.fault }
func record(kind, id string, v any) store.Record {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return store.Record{Type: kind, Version: 1, ID: id, Payload: raw}
}
func (m *Manager) event(kind, trace, turn string, v any) agent.Event {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return agent.Event{SchemaVersion: 1, Type: kind, Scope: agent.EventScope{SessionID: m.sessionID, TraceID: trace, TurnID: turn}, EventID: agent.MustID(), OccurredAt: time.Now().UTC(), Payload: raw}
}

// commit validates a private candidate. Neither state nor events become visible before Append succeeds.
func (m *Manager) commit(ctx context.Context, controls, entries []store.Record, events []agent.Event) (store.CommitReceipt, error) {
	if m.fault != nil {
		return store.CommitReceipt{}, m.fault
	}
	if m.view.RepairRequired {
		return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "journal requires repair")
	}
	if err := ctx.Err(); err != nil {
		return store.CommitReceipt{}, err
	}
	c := store.Commit{RecordType: "commit", Version: 1, CommitID: agent.MustID(), CommitSeq: m.view.LastSeq + 1, ExpectedPreviousSeq: m.view.LastSeq, ControlRecords: controls, Entries: entries, Events: events}
	for i := range c.Events {
		seq := m.view.Cursor + uint64(i) + 1
		c.Events[i].DurableSeq = &seq
	}
	candidate := clone(*m.view)
	if err := applyCommit(&candidate, c); err != nil {
		return store.CommitReceipt{}, err
	}
	receipt, err := m.store.Append(ctx, m.sessionID, store.ExpectedCommit{ExpectedPreviousSeq: m.view.LastSeq}, c)
	if err != nil {
		m.fault = product.NewError(product.CodeStorageUnavailable, "session commit failed; reopen required")
		return store.CommitReceipt{}, err
	}
	m.view = &candidate
	return receipt, nil
}
