package codeagent

import (
	"encoding/json"
	"reflect"
	"slices"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	store "github.com/ww1489/seasprak/internal/storage"
)

// checkpointPendingTurnSelection permits control requests for a future Turn,
// never activation or edits to the checkpoint's original execution snapshot.
func checkpointPendingTurnSelection(cp state.CheckpointRef, commit store.Commit, rec store.Record, view state.View, definitions []tools.Definition) bool {
	var selected state.Selection
	scope := agent.ExecutionScope{SessionID: cp.Scope.SessionID, BranchID: cp.Scope.BranchID, TraceID: cp.Scope.TraceID, InvocationID: cp.Scope.InvocationID, Generation: cp.Scope.Generation}
	if json.Unmarshal(rec.Payload, &selected) != nil || rec.ID == "" || selected.ID != rec.ID || selected.ApplyAt != "next_turn" || selected.Scope != scope || (selected.Kind != "model" && selected.Kind != "tools") {
		return false
	}
	persisted, ok := view.Selections[selected.ID]
	if !ok || (persisted.State != "pending" && persisted.State != "superseded") {
		return false
	}
	persisted.State = selected.State
	if !reflect.DeepEqual(selected, persisted) {
		return false
	}
	kind := "select_next_turn_model"
	if selected.Kind == "tools" {
		kind = "select_active_tools"
	}
	op, ok := view.Operations[selected.OperationID]
	if !ok || op.Kind != kind || op.SessionID != cp.Scope.SessionID || op.Receipt.OperationID != selected.OperationID || op.Receipt.Target != cp.Scope.TraceID || op.Receipt.AcceptedCommit != selected.Revision || op.State != "accepted" || op.Revision != 1 {
		return false
	}
	if !state.SelectionMatchesOperation(selected, op) {
		return false
	}
	if selected.Kind == "tools" {
		// Versions are generation-derived, not request content. Validate against
		// the host inventory already checked against the checkpoint manifest;
		// comparing two projections of the same journal provides no evidence.
		if len(selected.ToolNames) != len(selected.ToolVersions) {
			return false
		}
		for i, name := range selected.ToolNames {
			index := slices.IndexFunc(definitions, func(def tools.Definition) bool { return def.Name == name })
			if index < 0 || definitions[index].Version != selected.ToolVersions[i] {
				return false
			}
		}
	}
	switch selected.State {
	case "pending":
		if selected.Revision != commit.CommitSeq {
			return false
		}
		for _, record := range commit.ControlRecords {
			if record.Type != "operation" || record.ID != selected.OperationID {
				continue
			}
			var accepted state.Operation
			return json.Unmarshal(record.Payload, &accepted) == nil && reflect.DeepEqual(accepted, op)
		}
	case "superseded":
		if selected.Revision >= commit.CommitSeq {
			return false
		}
		for _, record := range commit.ControlRecords {
			var next state.Selection
			if record.Type == "selection" && json.Unmarshal(record.Payload, &next) == nil && next.Kind == selected.Kind && next.State == "pending" && next.Supersedes == selected.ID && checkpointPendingTurnSelection(cp, commit, record, view, definitions) {
				return true
			}
		}
	}
	return false
}
