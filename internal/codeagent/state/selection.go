package state

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
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

// SelectionMatchesOperation reconstructs the existing acceptance command. Older
// tool selections used the same digest shape and remain compatible; the caller
// must separately match ToolVersions against the fixed trusted generation,
// because tool versions were never part of the request digest. InvocationID was
// optional in the request, so only omitted or the committed scope can match.
func SelectionMatchesOperation(selected Selection, op Operation) bool {
	if selected.Revision == 0 || selected.OperationID != op.Receipt.OperationID || selected.Revision != op.Receipt.AcceptedCommit || selected.Scope.SessionID != op.SessionID {
		return false
	}
	command := OperationCommand{Kind: op.Kind, Target: op.Receipt.Target, ExpectedRevision: selected.Revision - 1}
	if selected.ApplyAt == "next_trace" {
		scope := agent.ExecutionScope{SessionID: selected.Scope.SessionID, BranchID: selected.Scope.BranchID, Generation: selected.Scope.Generation}
		if selected.Kind != "model" || op.Kind != "select_default_model" || op.Receipt.Target != "next_trace" || selected.Scope != scope {
			return false
		}
		command.Content, _ = json.Marshal(struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}{selected.ModelName, selected.ModelVersion})
		digest, err := operationDigest(command)
		return err == nil && digest == op.Digest
	}
	if selected.ApplyAt != "next_turn" || op.Receipt.Target != selected.Scope.TraceID {
		return false
	}
	if selected.Kind == "model" && op.Kind == "select_next_turn_model" {
		command.Content, _ = json.Marshal(struct {
			TraceID string `json:"traceId"`
			Name    string `json:"name"`
			Version string `json:"version"`
		}{selected.Scope.TraceID, selected.ModelName, selected.ModelVersion})
		digest, err := operationDigest(command)
		return err == nil && digest == op.Digest
	}
	if selected.Kind != "tools" || op.Kind != "select_active_tools" {
		return false
	}
	for _, invocation := range []string{"", selected.Scope.InvocationID} {
		command.Content, _ = json.Marshal(struct {
			TraceID      string   `json:"traceId"`
			Names        []string `json:"names"`
			InvocationID string   `json:"invocationId,omitempty"`
		}{selected.Scope.TraceID, selected.ToolNames, invocation})
		digest, err := operationDigest(command)
		if err == nil && digest == op.Digest {
			return true
		}
	}
	return false
}

// BeginSelectedTurn commits the selected inventory, model activation, logical
// budget reservation and initial Turn snapshot in one journal transaction.
func (m *Manager) BeginSelectedTurn(ctx context.Context, scope agent.ExecutionScope, usage agent.Usage, turn agent.TurnRecord, selectionIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	trace := m.view.Traces[scope.TraceID]
	if trace == nil || trace.State != "running" || scope.SessionID != m.sessionID || scope.BranchID != m.view.BranchID || scope.InvocationID != trace.InvocationID || scope.Generation != trace.Generation {
		return product.NewError(product.CodeStateConflict, "turn selection scope does not match the active trace")
	}
	if turn.ID == "" || turn.ID != usage.ModelCallID || turn.TraceID != trace.ID || turn.InvocationID != trace.InvocationID || turn.SelectionRevision != m.view.LastSeq || turn.Ended || len(turn.CallIDs) != 0 || turn.TransportRequests != 0 {
		return product.NewError(product.CodeStateConflict, "initial turn snapshot is invalid")
	}
	if _, exists := m.view.Turns[turn.ID]; exists {
		return product.NewError(product.CodeStateConflict, "selected turn already exists")
	}
	if previous, ok := m.view.Turns[trace.Usage.ModelCallID]; ok && !previous.Ended {
		return product.NewError(product.CodeStateConflict, "previous turn is unfinished")
	}
	var activations []Selection
	seen := map[string]bool{}
	for _, id := range selectionIDs {
		selected, ok := m.view.Selections[id]
		if !ok || seen[selected.Kind] || selected.ApplyAt != "next_turn" || (selected.State != "pending" && selected.State != "active") || selected.Scope.SessionID != scope.SessionID || selected.Scope.BranchID != scope.BranchID || selected.Scope.TraceID != scope.TraceID || selected.Scope.Generation != scope.Generation || (selected.Scope.InvocationID != "" && selected.Scope.InvocationID != scope.InvocationID) {
			return product.NewError(product.CodeStateConflict, "selection does not belong to the new turn")
		}
		seen[selected.Kind] = true
		if selected.Kind == "model" && turn.ModelConfigVersion != "" && selected.ModelVersion != turn.ModelConfigVersion || selected.Kind == "tools" && !slices.Equal(selected.ToolNames, turn.ToolNames) {
			return product.NewError(product.CodeStateConflict, "selection differs from the new turn snapshot")
		}
		if selected.State == "pending" {
			selected.State = "active"
			activations = append(activations, selected)
		}
	}
	return m.saveTraceBudget(ctx, trace.ID, usage, &turn, activations)
}

// ActivateSelection transitions a single pending control record. Execution uses
// BeginSelectedTurn so activation cannot outlive a failed Turn reservation.
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
