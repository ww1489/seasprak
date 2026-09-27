package state

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// AcceptSelection commits the operation receipt and pending control fact in
// one journal commit. Replacing an unapplied selection records that fact in the
// same commit, so replay cannot observe two pending choices.
func (m *Manager) AcceptSelection(ctx context.Context, cmd OperationCommand, selection Selection) (OperationReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cmd.Kind == "" || cmd.Target == "" {
		return OperationReceipt{}, product.NewError(product.CodeInvalidArgument, "selection operation kind and target are required")
	}
	digest, err := operationDigest(cmd)
	if err != nil {
		return OperationReceipt{}, err
	}
	if receipt, found, err := m.findOperation(cmd); found || err != nil {
		return receipt, err
	}
	if cmd.ExpectedRevision != m.view.LastSeq {
		return OperationReceipt{}, product.NewError(product.CodeStateConflict, "session revision changed")
	}
	if selection.ID == "" || selection.Scope.SessionID != m.sessionID || selection.State != "pending" || (selection.Kind != "model" && selection.Kind != "tools") {
		return OperationReceipt{}, product.NewError(product.CodeInvalidArgument, "pending selection is invalid")
	}
	if selection.Kind == "model" && (selection.ModelName == "" || selection.ModelVersion == "") {
		return OperationReceipt{}, product.NewError(product.CodeInvalidArgument, "model selection identity is required")
	}
	var superseded *Selection
	if selection.Supersedes != "" {
		old, ok := m.view.Selections[selection.Supersedes]
		if !ok || old.State != "pending" || old.Kind != selection.Kind || !sameSelectionScope(old.Scope, selection.Scope) {
			return OperationReceipt{}, product.NewError(product.CodeStateConflict, "selection to supersede is not pending")
		}
		copy := old
		superseded = &copy
	} else {
		for _, old := range m.view.Selections {
			if old.State == "pending" && old.Kind == selection.Kind && old.ApplyAt == selection.ApplyAt && sameSelectionScope(old.Scope, selection.Scope) {
				copy := old
				superseded = &copy
				selection.Supersedes = old.ID
				break
			}
		}
	}
	receipt := OperationReceipt{OperationID: agent.MustID(), State: "accepted", Target: cmd.Target, AcceptedCommit: m.view.LastSeq + 1}
	op := Operation{Receipt: receipt, Principal: cmd.Principal, SessionID: m.sessionID, Kind: cmd.Kind, Key: cmd.IdempotencyKey, Digest: digest, Revision: 1, State: "accepted"}
	selection.OperationID = receipt.OperationID
	selection.Revision = receipt.AcceptedCommit
	controls := []store.Record{record("operation", receipt.OperationID, op)}
	if superseded != nil {
		superseded.State = "superseded"
		controls = append(controls, record("selection", superseded.ID, *superseded))
	}
	controls = append(controls, record("selection", selection.ID, selection))
	if _, err := m.commit(ctx, controls, nil, []agent.Event{m.event("selection.changed", selection.Scope.TraceID, selection.Scope.TurnID, selection)}); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// ActivateSelection is the only transition that makes a pending selection
// visible to a new Turn. It is deliberately separate from acceptance.
func (m *Manager) ActivateSelection(ctx context.Context, id string) (Selection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.view.Selections[id]
	if !ok {
		return Selection{}, product.NewError(product.CodeNotFound, "selection not found")
	}
	if old.State == "active" {
		return old, nil
	}
	if old.State != "pending" {
		return Selection{}, product.NewError(product.CodeStateConflict, "selection is not pending")
	}
	next := old
	next.State = "active"
	if _, err := m.commit(ctx, []store.Record{record("selection", id, next)}, nil, []agent.Event{m.event("selection.activated", old.Scope.TraceID, old.Scope.TurnID, next)}); err != nil {
		return Selection{}, err
	}
	return next, nil
}

func sameSelectionScope(a, b agent.ExecutionScope) bool {
	a.ExecutionID, a.TurnID = "", ""
	b.ExecutionID, b.TurnID = "", ""
	return a == b
}

func applySelection(v *View, r store.Record) error {
	var value Selection
	if err := json.Unmarshal(r.Payload, &value); err != nil {
		return err
	}
	if r.ID == "" || r.ID != value.ID || value.Kind == "" || value.State == "" || value.Scope.SessionID == "" {
		return product.NewError(product.CodeIncompatibleVersion, "invalid selection record")
	}
	if value.Kind != "model" && value.Kind != "tools" {
		return product.NewError(product.CodeIncompatibleVersion, "unknown selection kind")
	}
	if old, ok := v.Selections[r.ID]; ok {
		oldCore, nextCore := old, value
		oldCore.State, nextCore.State = "", ""
		if !reflect.DeepEqual(oldCore, nextCore) {
			return product.NewError(product.CodeStateConflict, "selection identity is immutable")
		}
		if old.State == value.State {
			return nil
		}
		if old.State != "pending" || (value.State != "active" && value.State != "superseded") {
			return product.NewError(product.CodeStateConflict, "invalid selection transition")
		}
	}
	if value.State != "pending" && value.State != "active" && value.State != "superseded" {
		return product.NewError(product.CodeIncompatibleVersion, "unknown selection state")
	}
	if v.Selections == nil {
		v.Selections = map[string]Selection{}
	}
	v.Selections[r.ID] = value
	return nil
}
