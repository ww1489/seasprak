package state

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

func (m *Manager) SaveTraceBudget(ctx context.Context, id string, usage agent.Usage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[id]
	if tr == nil {
		return product.NewError(product.CodeNotFound, "trace not found")
	}
	next := *tr
	next.Usage = usage
	_, err := m.commit(ctx, []store.Record{record("trace", id, next)}, nil, nil)
	return err
}
func (m *Manager) SaveBudget(ctx context.Context, usage agent.Usage) error {
	v := m.View()
	if v.ActiveTrace == "" {
		return product.NewError(product.CodeStateConflict, "no active trace")
	}
	return m.SaveTraceBudget(ctx, v.ActiveTrace, usage)
}
