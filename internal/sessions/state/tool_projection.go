package state

import (
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// SaveToolProjection attaches presentation without changing the observation,
// claim, budget, reconciliation evidence, or resource holds.
func (m *Manager) SaveToolProjection(ctx context.Context, p agent.ToolOutputProjection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateToolProjection(m.view, p); err != nil {
		return err
	}
	if old, ok := m.view.ToolProjections[p.CallID]; ok {
		if old == p {
			return nil
		}
		return product.NewError(product.CodeStateConflict, "tool projection is immutable")
	}
	_, err := m.commit(ctx, []store.Record{record("tool_output_projection", p.CallID, p)}, nil, nil)
	return err
}

func validateToolProjection(v *View, p agent.ToolOutputProjection) error {
	call, ok := v.Calls[p.CallID]
	if !ok || p.CallID == "" || call.Observation == nil || *call.Observation != p.Observation || !p.Observation.Process {
		return product.NewError(product.CodeStateConflict, "tool projection does not match original observation")
	}
	if p.Artifact.ID != "" && (!p.Artifact.Available || p.Artifact.SessionID != call.Scope.SessionID || p.Artifact.Environment == "" || p.Artifact.Hash == "" || p.Artifact.Size < 0 || p.LogError != "") {
		return product.NewError(product.CodeStateConflict, "tool projection artifact binding is invalid")
	}
	return nil
}

func applyToolProjection(v *View, r store.Record) error {
	var p agent.ToolOutputProjection
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return err
	}
	if r.ID != p.CallID {
		return product.NewError(product.CodeStateConflict, "tool projection identity mismatch")
	}
	if err := validateToolProjection(v, p); err != nil {
		return err
	}
	return putImmutable(&v.ToolProjections, r.ID, p.CallID, p)
}
