package sessions

import (
	"encoding/json"
	"reflect"

	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

func checkpointHostCommandResult(cp state.CheckpointRef, c store.Commit, r store.Record, v state.View) bool {
	if cp.Scope.SessionID == "" || cp.Scope.BranchID != v.BranchID || r.Type != "host_command_result" || c.CommitSeq <= cp.HistoryCommit || c.CommitSeq > v.LastSeq || v.HostCommandConsumptions[r.ID] != 0 {
		return false
	}
	if state.ValidateHostCommandCommit(v, c, cp.Scope.SessionID) != nil {
		return false
	}
	var result state.HostCommandResult
	saved, ok := v.HostCommands[r.ID]
	if !ok || json.Unmarshal(r.Payload, &result) != nil || !reflect.DeepEqual(saved, result) {
		return false
	}
	// Match the exact persisted display event too, not merely its type/payload.
	for _, event := range v.Events {
		if event.EventID == c.Events[0].EventID {
			return reflect.DeepEqual(event, c.Events[0])
		}
	}
	return false
}
