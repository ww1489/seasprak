package state

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

func terminal(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled"
}
func (m *Manager) SetTraceState(ctx context.Context, id, state string, settled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.view.Traces[id]
	if old == nil {
		return product.NewError(product.CodeNotFound, "trace not found")
	}
	if old.State == state {
		return nil
	}
	if terminal(old.State) {
		return product.NewError(product.CodeStateConflict, "terminal trace cannot change")
	}
	allowed := false
	switch old.State {
	case "queued":
		allowed = state == "running" || state == "cancelled" || state == "failed"
	case "running":
		allowed = state == "completed" || state == "failed" || state == "paused" || state == "cancelling"
	case "cancelling":
		allowed = state == "cancelled" || state == "paused" || state == "failed"
	case "paused":
		allowed = state == "cancelling" || state == "failed"
	}
	if !allowed {
		return product.NewError(product.CodeStateConflict, "invalid trace transition")
	}
	if state == "running" {
		if old.State != "queued" || m.view.ActiveTrace != "" {
			return product.NewError(product.CodeStateConflict, "top-level execution is occupied")
		}
	}
	tr := *old
	tr.State = state
	if state == "running" {
		if !old.Started && !tr.Activity.Unknown {
			tr.Activity.Known = true
		}
		tr.Started = true
		tr.ExecutionStopped = false
		tr.InvocationID = agent.MustID()
	}
	tr.Settled = terminal(state) && tr.Started
	if state == "cancelling" {
		for _, inID := range m.view.Independent {
			q := m.view.Inputs[inID]
			other := m.view.Traces[q.TraceID]
			if other.ID != id && other.State == "queued" {
				tr.HoldOnStop = append(tr.HoldOnStop, other.ID)
			}
		}
	}
	controls := []store.Record{record("trace", id, tr)}
	events := []agent.Event{m.event("trace.state_changed", id, "", tr)}
	if terminal(state) {
		if tr.Started {
			events = append(events, m.event("trace.settled", id, "", tr))
		}
		for _, inID := range m.view.Order {
			in := m.view.Inputs[inID]
			if in.TraceID == id && in.State == "pending" {
				next := *in
				next.State = "undelivered"
				controls = append(controls, record("input", in.ID, next))
			}
		}
		if state == "failed" {
			for _, inID := range m.view.Independent {
				other := m.view.Traces[m.view.Inputs[inID].TraceID]
				if other.ID != id && other.State == "queued" {
					tr.HoldOnStop = append(tr.HoldOnStop, other.ID)
				}
			}
		}
		for _, heldID := range tr.HoldOnStop {
			if other := m.view.Traces[heldID]; other != nil && other.State == "queued" {
				next := *other
				next.Hold = true
				controls = append(controls, record("trace", next.ID, next))
			}
		}
		events = append(events, m.event("queue.changed", id, "", map[string]string{"reason": state}))
	}
	_, err := m.commit(ctx, controls, nil, events)
	return err
}
func (m *Manager) HoldIndependent(ctx context.Context, id string) error {
	return m.setHold(ctx, id, true)
}
func (m *Manager) ReleaseHold(ctx context.Context, id string) error { return m.setHold(ctx, id, false) }
func (m *Manager) setHold(ctx context.Context, id string, hold bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[id]
	if tr == nil || tr.State != "queued" {
		return product.NewError(product.CodeStateConflict, "only queued trace can change hold")
	}
	next := *tr
	next.Hold = hold
	_, err := m.commit(ctx, []store.Record{record("trace", id, next)}, nil, []agent.Event{m.event("queue.changed", id, "", next)})
	return err
}
func (m *Manager) SaveTraceError(ctx context.Context, id, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[id]
	if tr == nil {
		return product.NewError(product.CodeNotFound, "trace not found")
	}
	next := *tr
	next.Error = message
	_, err := m.commit(ctx, []store.Record{record("trace", id, next)}, nil, nil)
	return err
}
