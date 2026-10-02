package codeagent

import (
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func TestP2CommandCheckpointRejectsForgedResultProgress(t *testing.T) {
	f := waitingApprovalSession(t, nil)
	msg := agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindCommand, Status: agent.StatusComplete, Scope: agent.MessageScope{SessionID: f.s.rt.opts.SessionID}, Source: agent.SourceRef{Kind: agent.SourceHuman, Description: "host shell"}, Command: &agent.CommandMessage{Name: "shell", Content: json.RawMessage(`{"command":"fixture"}`), Result: json.RawMessage(`{"output":"fixture","exitCode":0,"started":true,"terminated":true}`)}}
	if err := f.s.rt.do(t.Context(), func(rt *runtime) error { return rt.manager.SaveHostCommand(t.Context(), msg, false) }); err != nil {
		t.Fatal(err)
	}
	view := f.manager.View()
	cp := view.Checkpoints[view.Traces[f.input.TraceID].CheckpointID]
	for _, field := range []string{"valid", "id", "session", "branch", "sequence", "version", "parent", "event", "event-id", "mixed-entry", "mixed-control", "branch-update", "result", "consumed"} {
		t.Run(field, func(t *testing.T) {
			stored, err := f.s.rt.opts.Store.Load(t.Context(), f.s.rt.opts.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			c := &stored.Commits[len(stored.Commits)-1]
			var result state.HostCommandResult
			if err := json.Unmarshal(c.ControlRecords[0].Payload, &result); err != nil {
				t.Fatal(err)
			}
			current := f.manager.View()
			switch field {
			case "id":
				c.ControlRecords[0].ID = "other"
			case "session":
				result.Message.Scope.SessionID = "other"
			case "branch":
				result.BranchID = "other"
			case "sequence":
				result.CommitSeq++
			case "version":
				c.ControlRecords[0].Version++
			case "parent":
				c.ControlRecords[0].ParentID = cp.LeafID
			case "event":
				c.Events[0].Scope.TraceID = f.input.TraceID
			case "event-id":
				c.Events[0].EventID = agent.MustID()
			case "mixed-entry":
				c.Entries = []store.Record{{Type: "message", Version: 1, ID: "forged"}}
			case "mixed-control":
				c.ControlRecords = append(c.ControlRecords, store.Record{Type: "trace", Version: 1, ID: "forged"})
			case "branch-update":
				c.BranchUpdates = []store.BranchUpdate{{BranchID: "other"}}
			case "result":
				result.Message.Command.Result = json.RawMessage(`{"output":"changed","exitCode":0,"started":true,"terminated":true}`)
			case "consumed":
				current.HostCommandConsumptions = map[string]uint64{msg.ID: c.CommitSeq}
			}
			c.ControlRecords[0].Payload, _ = json.Marshal(result)
			err = validateCheckpointProgress(cp, stored, current, f.s.rt.opts.Tools)
			if field == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireSessionCode(t, err, product.CodeIncompatibleResume)
			}
		})
	}
}
